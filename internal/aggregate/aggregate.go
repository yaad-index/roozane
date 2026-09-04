// Package aggregate is layer 2: the only layer with a brain, and stiff by
// design (ADR-0001 §3).
//
// It reads the day's collected items one at a time, judges each against the
// reader's own relevance profile, and writes a digest from what survives. Two
// properties are load-bearing and are enforced here rather than left to the
// prompt: suppression is the default, and a day where nothing clears the bar
// produces an explicit empty digest rather than filler.
package aggregate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/llm"
	"github.com/yaad-index/roozane/internal/store"
)

// Schema versions for the two files this package owns. ADR-0002 requires the
// digest to carry one from day one, since sinks depend on its shape.
const (
	DigestSchema = 1
	stateSchema  = 1
)

// Per-item outcomes recorded in state.json.
const (
	StatusRelevant   = "relevant"
	StatusSuppressed = "suppressed"
	StatusFailed     = "failed"
)

// Local aliases so the prompt builders read in this package's own terms and do
// not have to care which client type is underneath.
type llmMessages = llm.Message

const (
	roleSystem = llm.RoleSystem
	roleUser   = llm.RoleUser
)

// Judgement is the first pass's verdict on one item.
type Judgement struct {
	Relevant bool     `json:"relevant"`
	Score    float64  `json:"score"`
	Points   []string `json:"points"`
	Reason   string   `json:"reason"`
}

type judgedItem struct {
	Item      store.StoredItem
	Judgement Judgement
}

// ItemState is one item's line in the day's bookkeeping. Keeping the judgement
// itself here is what makes a re-run resume rather than re-pay: the digest can
// be rebuilt from state without calling the model again.
type ItemState struct {
	Status    string     `json:"status"`
	Model     string     `json:"model,omitempty"`
	Usage     llm.Usage  `json:"usage"`
	Error     string     `json:"error,omitempty"`
	Judgement *Judgement `json:"judgement,omitempty"`
}

// State is `days/<day>/state.json` (ADR-0002 §5).
type State struct {
	Schema    int                  `json:"schema"`
	Day       string               `json:"day"`
	UpdatedAt string               `json:"updated_at"`
	Items     map[string]ItemState `json:"items"`
}

// DigestItem is one entry in the structured digest a sink consumes.
type DigestItem struct {
	Source string   `json:"source"`
	URL    string   `json:"url,omitempty"`
	Title  string   `json:"title,omitempty"`
	Score  float64  `json:"score"`
	Reason string   `json:"reason"`
	Points []string `json:"points"`
}

// Digest is `digests/<day>.json`.
type Digest struct {
	Schema      int    `json:"schema"`
	Day         string `json:"day"`
	GeneratedAt string `json:"generated_at"`

	// Empty says a run happened and nothing cleared the bar. It is written
	// rather than implied by an absent file, so "quiet day" and "the aggregator
	// never ran" stay distinguishable (ADR-0002 §4).
	Empty bool `json:"empty"`

	Items []DigestItem `json:"items"`
}

// emptyDigestMarker is the human-readable counterpart of Empty. It states the
// outcome plainly: a reader who sees it knows the engine ran and found nothing,
// which is a correct result rather than a broken one.
const emptyDigestMarker = "_Nothing today cleared the relevance bar._"

// Completer is the piece of the LLM client this package needs. Narrow, so a
// test supplies a function rather than a server.
type Completer interface {
	Complete(ctx context.Context, req llm.Request) (llm.Response, error)
}

// Runner performs one aggregation pass over a day.
type Runner struct {
	cfg    *config.Config
	store  *store.Store
	client Completer
	log    *slog.Logger
	now    func() time.Time
}

// Option customises a Runner for tests.
type Option func(*Runner)

// WithClient replaces the completions client.
func WithClient(c Completer) Option { return func(r *Runner) { r.client = c } }

// WithClock replaces the wall clock, which stamps the digest.
func WithClock(now func() time.Time) Option { return func(r *Runner) { r.now = now } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(r *Runner) { r.log = l } }

// NewRunner builds a Runner over a validated config. The credential is read
// from the environment variable the config names — never from the config.
func NewRunner(cfg *config.Config, opts ...Option) (*Runner, error) {
	r := &Runner{
		cfg:   cfg,
		store: store.New(cfg.DataRootPath()),
		log:   slog.Default(),
		now:   time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}

	if r.client == nil {
		key, err := cfg.APIKey()
		if err != nil {
			return nil, err
		}
		r.client = llm.New(cfg.Aggregator.BaseURL, key, cfg.Aggregator.RequestTimeout())
	}
	return r, nil
}

// Result reports what a pass did.
type Result struct {
	Day        string
	Items      int
	Relevant   int
	Suppressed int
	Failed     int
	Reused     int // answered from state instead of a new call
	Empty      bool
	Usage      llm.Usage
}

// Run aggregates one day.
//
// A failing item is recorded and skipped rather than aborting: one item the
// model could not judge must not cost the reader the rest of the digest. The
// pass still reports the failure count so a caller can exit non-zero.
func (r *Runner) Run(ctx context.Context, day time.Time) (Result, error) {
	result := Result{Day: store.Day(day)}

	profile, err := r.readProfile()
	if err != nil {
		return result, err
	}

	items, err := r.store.ReadItems(day)
	if err != nil {
		return result, fmt.Errorf("read items: %w", err)
	}
	result.Items = len(items)

	state := r.loadState(day)

	var relevant []judgedItem
	for _, item := range items {
		judgement, prior, err := r.judge(ctx, profile, item, state)
		if err != nil {
			result.Failed++
			state.Items[item.Filename] = ItemState{Status: StatusFailed, Model: r.cfg.Aggregator.Models.Item, Error: err.Error()}
			r.log.Error("item judgement failed", "item", item.Filename, "error", err)
			continue
		}
		if prior {
			result.Reused++
		} else {
			result.Usage.PromptTokens += judgement.usage.PromptTokens
			result.Usage.CompletionTokens += judgement.usage.CompletionTokens
			result.Usage.TotalTokens += judgement.usage.TotalTokens
		}

		status := StatusSuppressed
		if judgement.verdict.Relevant {
			status = StatusRelevant
			relevant = append(relevant, judgedItem{Item: item, Judgement: judgement.verdict})
			result.Relevant++
		} else {
			result.Suppressed++
		}

		verdict := judgement.verdict
		state.Items[item.Filename] = ItemState{
			Status:    status,
			Model:     r.cfg.Aggregator.Models.Item,
			Usage:     judgement.usage,
			Judgement: &verdict,
		}
	}

	markdown, usage, err := r.writeDigest(ctx, day, profile, relevant)
	if err != nil {
		// State is still worth keeping: the per-item judgements were paid for
		// and a re-run should not buy them twice just because the digest call
		// failed.
		r.saveState(day, state)
		return result, err
	}
	result.Usage.PromptTokens += usage.PromptTokens
	result.Usage.CompletionTokens += usage.CompletionTokens
	result.Usage.TotalTokens += usage.TotalTokens
	result.Empty = len(relevant) == 0

	r.saveState(day, state)
	r.log.Info("digest written",
		"day", result.Day, "items", result.Items, "relevant", result.Relevant,
		"suppressed", result.Suppressed, "failed", result.Failed, "reused", result.Reused,
		"empty", result.Empty, "bytes", len(markdown))

	return result, nil
}

// readProfile reads the reader-owned relevance profile verbatim. The engine
// never infers, rewrites or caches an interpretation of it: it is the one input
// the reader fully controls (ADR-0001).
func (r *Runner) readProfile() (string, error) {
	path := r.cfg.RelevanceProfilePath()

	raw, err := os.ReadFile(path) //nolint:gosec // path comes from the operator's own config
	if err != nil {
		return "", fmt.Errorf("read relevance profile %s: %w", path, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		// An empty profile matches nothing, so every item would be suppressed
		// and every digest would be empty — a silent, confusing no-op. Say so.
		return "", fmt.Errorf("relevance profile %s is empty: it is what the engine judges against, so nothing would ever be relevant", path)
	}
	return string(raw), nil
}

// judgeResult carries a verdict with the accounting for the call that produced
// it, so a reused judgement can be told apart from a paid-for one.
type judgeResult struct {
	verdict Judgement
	usage   llm.Usage
}

// judge returns one item's verdict, reusing a recorded one when the item has
// already been judged in a previous pass. The second result reports whether it
// came from state.
func (r *Runner) judge(ctx context.Context, profile string, item store.StoredItem, state State) (judgeResult, bool, error) {
	if prior, ok := state.Items[item.Filename]; ok && prior.Judgement != nil && prior.Status != StatusFailed {
		return judgeResult{verdict: *prior.Judgement, usage: prior.Usage}, true, nil
	}

	resp, err := r.client.Complete(ctx, llm.Request{
		Model:    r.cfg.Aggregator.Models.Item,
		Messages: buildItemMessages(profile, item),
	})
	if err != nil {
		return judgeResult{}, false, err
	}

	verdict, err := parseJudgement(resp.Content)
	if err != nil {
		return judgeResult{}, false, err
	}
	return judgeResult{verdict: verdict, usage: resp.Usage}, false, nil
}

// writeDigest renders and writes both digest files, returning the markdown.
//
// A day with nothing relevant skips the model entirely: there is nothing to
// write a digest from, the correct output is the empty marker, and asking a
// model to write about nothing is exactly how filler gets produced.
func (r *Runner) writeDigest(ctx context.Context, day time.Time, profile string, relevant []judgedItem) (string, llm.Usage, error) {
	var markdown string
	var usage llm.Usage

	if len(relevant) == 0 {
		markdown = fmt.Sprintf("# Digest — %s\n\n%s\n", store.Day(day), emptyDigestMarker)
	} else {
		resp, err := r.client.Complete(ctx, llm.Request{
			Model:    r.cfg.Aggregator.Models.Digest,
			Messages: buildDigestMessages(profile, relevant),
		})
		if err != nil {
			return "", usage, fmt.Errorf("digest pass: %w", err)
		}
		usage = resp.Usage
		body := strings.TrimSpace(resp.Content)
		if body == "" {
			return "", usage, errors.New("digest pass returned no text")
		}
		markdown = fmt.Sprintf("# Digest — %s\n\n%s\n", store.Day(day), body)
	}

	digest := Digest{
		Schema:      DigestSchema,
		Day:         store.Day(day),
		GeneratedAt: r.now().UTC().Format(time.RFC3339),
		Empty:       len(relevant) == 0,
		Items:       make([]DigestItem, 0, len(relevant)),
	}
	for _, j := range relevant {
		digest.Items = append(digest.Items, DigestItem{
			Source: j.Item.Source,
			URL:    j.Item.URL,
			Title:  j.Item.Title,
			Score:  j.Judgement.Score,
			Reason: j.Judgement.Reason,
			Points: j.Judgement.Points,
		})
	}

	structured, err := json.MarshalIndent(digest, "", "  ")
	if err != nil {
		return "", usage, fmt.Errorf("encode digest: %w", err)
	}

	mdPath, jsonPath := r.store.DigestPaths(day)
	if err := r.store.WriteAtomic(mdPath, []byte(markdown)); err != nil {
		return "", usage, fmt.Errorf("write digest markdown: %w", err)
	}
	if err := r.store.WriteAtomic(jsonPath, append(structured, '\n')); err != nil {
		return "", usage, fmt.Errorf("write digest json: %w", err)
	}
	return markdown, usage, nil
}

func (r *Runner) loadState(day time.Time) State {
	state := State{Schema: stateSchema, Day: store.Day(day), Items: map[string]ItemState{}}

	raw, err := os.ReadFile(r.store.StatePath(day)) //nolint:gosec // path is inside the engine's own data root
	if err != nil {
		return state
	}
	var loaded State
	if err := json.Unmarshal(raw, &loaded); err != nil {
		// A corrupt state file costs a re-run, not the pass. Say so and start
		// clean rather than failing on bookkeeping.
		r.log.Warn("state file unreadable, starting the day fresh", "error", err)
		return state
	}
	if loaded.Items == nil {
		loaded.Items = map[string]ItemState{}
	}
	loaded.Schema = stateSchema
	loaded.Day = state.Day
	return loaded
}

func (r *Runner) saveState(day time.Time, state State) {
	state.UpdatedAt = r.now().UTC().Format(time.RFC3339)

	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		r.log.Error("could not encode state", "error", err)
		return
	}
	if err := r.store.WriteAtomic(r.store.StatePath(day), append(raw, '\n')); err != nil {
		// Bookkeeping failing does not invalidate a written digest, so this is
		// reported rather than raised: the cost is a re-run paying twice.
		r.log.Error("could not write state", "error", err)
	}
}

// parseJudgement decodes the first pass's reply.
//
// Models routinely wrap JSON in a fenced code block despite being asked not to,
// so the fence is stripped before decoding rather than treated as a failure —
// the alternative is discarding a correct answer over its packaging.
func parseJudgement(content string) (Judgement, error) {
	text := strings.TrimSpace(content)

	if strings.HasPrefix(text, "```") {
		if end := strings.LastIndex(text, "```"); end > 3 {
			inner := text[3:end]
			if nl := strings.IndexByte(inner, '\n'); nl >= 0 {
				inner = inner[nl+1:] // drop the language tag line
			}
			text = strings.TrimSpace(inner)
		}
	}

	var j Judgement
	if err := json.Unmarshal([]byte(text), &j); err != nil {
		return Judgement{}, fmt.Errorf("item pass did not return usable JSON: %w (got: %s)", err, snippet(text))
	}
	if j.Relevant && len(j.Points) == 0 {
		// An item with nothing extractable cannot contribute to a digest, so
		// calling it relevant is a contradiction the digest pass would have to
		// paper over.
		return Judgement{}, errors.New("item pass marked an item relevant but returned no data points")
	}
	return j, nil
}

func snippet(s string) string {
	const limit = 200
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}
