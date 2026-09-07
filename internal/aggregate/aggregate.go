// Package aggregate is layer 2: the only layer with a brain, and stiff by
// design (ADR-0001 §3).
//
// It runs in two passes (ADR-0005). ENRICH reads each of the day's items once
// and records a neutral summary, tags, a category and a generic salience score
// — "is this substantive at all", never "does this reader care". SELECT then
// runs once per edition, narrowing by that edition's source list and judging
// the enriched summaries against that edition's own relevance profile, and
// writes that edition's digest.
//
// The split is what lets many audiences share one pool: because the per-item
// pass carries no audience, its result is the same for everybody, so it is paid
// for once and cached on the item filename alone. It also removes a leak by
// construction rather than by defending against one — with no audience in the
// per-item record, no private reasoning can reach a public digest.
//
// Two properties remain load-bearing and are enforced here rather than left to
// the prompt: suppression is the default, and an edition where nothing clears
// the bar produces an explicit empty digest rather than filler.
package aggregate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yaad-index/roozane/internal/collect"
	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/llm"
	"github.com/yaad-index/roozane/internal/store"
)

// Schema versions for the two files this package owns. ADR-0002 requires the
// digest to carry one from day one, since sinks depend on its shape.
//
// stateSchema is 2 because state.json now records a neutral enrichment where it
// used to record a per-reader judgement. The two are not convertible — the old
// record answers "does this reader care", which the new one deliberately never
// asks — so the existing schema-mismatch path starts the day fresh rather than
// relabelling one as the other. That costs one re-enriched day, not a converter.
//
// DigestSchema is 2 because the digest JSON gained the edition it was written
// for and the collection outcomes of the sources its edition drew on. Both are
// additive, so an existing reader keeps working — but a version whose shape has
// changed underneath it tells a reader nothing, which is the whole job of
// carrying one.
const (
	DigestSchema = 2
	stateSchema  = 2

	// enrichPromptVersion is bumped whenever the enrichment prompt changes in a
	// way that would produce a different result. It is recorded alongside the
	// model so a cached enrichment can be invalidated by either: the cache key
	// is the item, but the cache's validity depends on what produced it.
	enrichPromptVersion = 1
)

// Per-item outcomes recorded in state.json. Enrichment is audience-agnostic, so
// there is no "relevant" or "suppressed" here any more — whether an item is
// wanted is a question only an edition can ask, and it is asked in the select
// pass, which writes no per-item state.
const (
	StatusEnriched = "enriched"
	StatusFailed   = "failed"
)

// Local aliases so the prompt builders read in this package's own terms and do
// not have to care which client type is underneath.
type llmMessages = llm.Message

const (
	roleSystem = llm.RoleSystem
	roleUser   = llm.RoleUser
)

// Enrichment is the neutral pass's reading of one item. Nothing in it refers to
// a reader: Salience is "is this substantive at all", never "does this reader
// care", which is what makes the same record correct for every edition.
type Enrichment struct {
	Summary  string   `json:"summary"`
	Points   []string `json:"points"`
	Tags     []string `json:"tags"`
	Category string   `json:"category"`
	Salience float64  `json:"salience"`
}

// Selection is one edition's verdict on one enriched item. It is deliberately
// not recorded in state.json: it is per (item, edition), and putting an audience
// dimension into the per-item record is exactly what the neutral pass exists to
// avoid.
type Selection struct {
	Selected bool    `json:"selected"`
	Score    float64 `json:"score"`
	Reason   string  `json:"reason"`
}

// enrichedItem pairs an item with its neutral reading, which is what a
// selection pass sees instead of the full item.
type enrichedItem struct {
	Item       store.StoredItem
	Enrichment Enrichment
}

// selectedItem is an enriched item one edition chose, with that edition's
// reasoning attached.
type selectedItem struct {
	Item       store.StoredItem
	Enrichment Enrichment
	Selection  Selection
}

// ItemState is one item's line in the day's bookkeeping. Keeping the enrichment
// itself here is what makes a re-run resume rather than re-pay, and keeping it
// audience-free is what lets every edition reuse the one record.
type ItemState struct {
	Status string    `json:"status"`
	Model  string    `json:"model,omitempty"`
	Usage  llm.Usage `json:"usage"`
	Error  string    `json:"error,omitempty"`

	// PromptVersion records which enrichment prompt produced this result, so a
	// changed prompt invalidates the cache as surely as a changed model does.
	// Without it a prompt edit would be silently served stale results forever,
	// which is worse than a model change because nothing about the config looks
	// different.
	PromptVersion int `json:"prompt_version,omitempty"`

	Enrichment *Enrichment `json:"enrichment,omitempty"`
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
	Source   string   `json:"source"`
	URL      string   `json:"url,omitempty"`
	Title    string   `json:"title,omitempty"`
	Score    float64  `json:"score"`
	Reason   string   `json:"reason"`
	Points   []string `json:"points"`
	Tags     []string `json:"tags,omitempty"`
	Category string   `json:"category,omitempty"`
}

// Digest is `digests/<edition>/<day>.json`.
type Digest struct {
	Schema int    `json:"schema"`
	Day    string `json:"day"`

	// Edition names which audience this digest was written for. The path
	// already encodes it, but a sink and any tooling downstream receive the
	// document rather than its location, and a digest that cannot say which
	// edition it is cannot be told apart from another edition's once it has
	// been handed on.
	Edition string `json:"edition"`

	GeneratedAt string `json:"generated_at"`

	// Empty says a run happened and nothing cleared the bar. It is written
	// rather than implied by an absent file, so "quiet day" and "the aggregator
	// never ran" stay distinguishable (ADR-0002 §4).
	Empty bool `json:"empty"`

	// Sources is what collection did today for the sources this edition drew
	// on, keyed by source id (ADR-0005 §8).
	//
	// It matters most when Items is empty. A narrow edition otherwise dilutes a
	// total upstream failure into something that reads exactly like a quiet
	// day: one source, that source down, nothing selected — indistinguishable
	// from one source that simply had nothing. It is carried on every digest
	// rather than only on empty ones, so the field's presence never has to be
	// interpreted as a signal in itself.
	//
	// This is the structural half of ADR-0005 §8's split: error text belongs
	// here, where sinks and tooling read it, and never in the markdown.
	Sources map[string]collect.SourceOutcome `json:"sources,omitempty"`

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

	// salienceFloor defaults to the package constant. It is a field so the
	// mechanism can be exercised at all: with the constant at its conservative
	// 0, no valid salience is ever below it, and a check nothing can trip is a
	// check nothing can test.
	salienceFloor float64
}

// Option customises a Runner for tests.
type Option func(*Runner)

// WithClient replaces the completions client.
func WithClient(c Completer) Option { return func(r *Runner) { r.client = c } }

// WithClock replaces the wall clock, which stamps the digest.
func WithClock(now func() time.Time) Option { return func(r *Runner) { r.now = now } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(r *Runner) { r.log = l } }

// WithSalienceFloor overrides the generic-salience bar. It exists so the floor's
// behaviour can be exercised while the shipped constant is 0, and so the run
// that eventually measures a real corpus can sweep candidate values without
// editing the source.
func WithSalienceFloor(floor float64) Option {
	return func(r *Runner) { r.salienceFloor = floor }
}

// NewRunner builds a Runner over a validated config. The credential is read
// from the environment variable the config names — never from the config.
func NewRunner(cfg *config.Config, opts ...Option) (*Runner, error) {
	r := &Runner{
		cfg:           cfg,
		store:         store.New(cfg.DataRootPath()),
		log:           slog.Default(),
		now:           time.Now,
		salienceFloor: salienceFloor,
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

// salienceFloor is the generic-salience bar an enriched item must clear to be
// offered to any edition (ADR-0005 §1). It exists to drop obvious junk before
// selection, which is the one place per-edition cost comes down: selection runs
// per (item, edition), so an item dropped here is dropped once for every
// audience.
//
// 🚨 It is deliberately 0, which drops nothing.
//
// A floor is a claim about a scale, and the scale is introduced by this same
// change — so any number chosen now would be justified by seeming reasonable
// rather than by the items it removes, which is not a justification. Setting it
// needs the enricher run over a real day's corpus, sorted by salience, with the
// candidate floor judged by what it actually drops. That measurement cannot be
// made from the source tree; it needs a live deployment's items.
//
// The asymmetry says which way to be wrong meanwhile: a floor too low costs
// some selection work, a floor too high silently removes something the reader
// wanted, and that loss is invisible to them. So it starts at the conservative
// limit and pays the compute.
//
// The mechanism ships regardless, because ADR-0005 §7 requires "below the
// generic salience floor" to be a reportable absence reason — with the check in
// place, a badly chosen number later shows up as a count in the report instead
// of as items quietly missing.
const salienceFloor = 0.0

// Result reports what a pass did.
type Result struct {
	Day   string
	Items int

	Enriched int
	Failed   int
	Reused   int // answered from state instead of a new call

	// BelowFloor counts items enriched successfully but held back from every
	// edition by salienceFloor.
	BelowFloor int

	Usage llm.Usage

	// Editions reports each edition's selection pass, in edition-id order.
	Editions []EditionResult
}

// EditionResult is one edition's outcome for the day.
type EditionResult struct {
	ID string

	// Candidates is how many enriched items this edition's source list admitted
	// and the salience floor let through — what it actually chose from.
	Candidates int
	Selected   int

	// Empty says this edition ran and selected nothing, which is a correct
	// outcome rather than a failure (ADR-0002 §4).
	Empty bool

	Usage llm.Usage
}

// Run aggregates one day.
//
// A failing item is recorded and skipped rather than aborting: one item the
// model could not judge must not cost the reader the rest of the digest. The
// pass still reports the failure count so a caller can exit non-zero.
func (r *Runner) Run(ctx context.Context, day time.Time) (Result, error) {
	result := Result{Day: store.Day(day)}

	items, err := r.store.ReadItems(day)
	if err != nil {
		return result, fmt.Errorf("read items: %w", err)
	}
	result.Items = len(items)

	state := r.loadState(day)

	// Pass one: enrich every item once, for nobody in particular.
	var enriched []enrichedItem
	for _, item := range items {
		outcome, prior, err := r.enrich(ctx, item, state)
		if err != nil {
			result.Failed++
			state.Items[item.Filename] = ItemState{
				Status:        StatusFailed,
				Model:         r.cfg.Aggregator.Models.Item,
				PromptVersion: enrichPromptVersion,
				Error:         err.Error(),
			}
			r.log.Error("item enrichment failed", "item", item.Filename, "error", err)
			continue
		}
		if prior {
			result.Reused++
		} else {
			addUsage(&result.Usage, outcome.usage)
		}
		result.Enriched++

		record := outcome.enrichment
		state.Items[item.Filename] = ItemState{
			Status:        StatusEnriched,
			Model:         r.cfg.Aggregator.Models.Item,
			PromptVersion: enrichPromptVersion,
			Usage:         outcome.usage,
			Enrichment:    &record,
		}

		if record.Salience < r.salienceFloor {
			result.BelowFloor++
			continue
		}
		enriched = append(enriched, enrichedItem{Item: item, Enrichment: record})
	}

	// Enrichment is saved before any selection runs. It was paid for, it is
	// audience-free, and a later edition failing must not cost it — otherwise
	// one broken profile makes every item be re-read on the next pass.
	r.saveState(day, state)

	// Pass two: select and write once per edition. An edition that fails does
	// not take the others down with it — its digest is the one lost, and the
	// error is reported after the rest have been written.
	var problems []error
	// Read once for the whole run: every edition reports a subset of the same
	// record, and it does not change while the pass is running.
	collected := r.loadCollectedOutcomes(day)

	for _, id := range sortedEditionIDs(r.cfg.Editions) {
		editionResult, err := r.runEdition(ctx, day, id, r.cfg.Editions[id], enriched, collected)
		result.Editions = append(result.Editions, editionResult)
		addUsage(&result.Usage, editionResult.Usage)
		if err != nil {
			problems = append(problems, fmt.Errorf("edition %q: %w", id, err))
			r.log.Error("edition failed", "edition", id, "error", err)
			continue
		}
		r.log.Info("digest written",
			"day", result.Day, "edition", id,
			"candidates", editionResult.Candidates, "selected", editionResult.Selected,
			"empty", editionResult.Empty)
	}

	r.log.Info("aggregation complete",
		"day", result.Day, "items", result.Items, "enriched", result.Enriched,
		"failed", result.Failed, "reused", result.Reused, "below_floor", result.BelowFloor,
		"editions", len(result.Editions))

	return result, errors.Join(problems...)
}

// runEdition performs one edition's selection pass and writes its digest.
func (r *Runner) runEdition(ctx context.Context, day time.Time, id string, edition config.Edition, enriched []enrichedItem, collected map[string]collect.SourceOutcome) (EditionResult, error) {
	editionResult := EditionResult{ID: id}

	profilePath, ok := r.cfg.EditionProfilePath(id)
	if !ok {
		// Unreachable through Load, which materialises every edition it
		// validates. Reported rather than ignored so a hand-built Config fails
		// loudly instead of writing a digest judged against nothing.
		return editionResult, fmt.Errorf("no profile for edition %q", id)
	}
	profile, err := readProfile(profilePath)
	if err != nil {
		return editionResult, err
	}

	candidates := admittedBySources(enriched, edition)
	editionResult.Candidates = len(candidates)

	var selected []selectedItem
	for _, candidate := range candidates {
		selection, usage, err := r.selectItem(ctx, profile, candidate)
		if err != nil {
			return editionResult, fmt.Errorf("select %s: %w", candidate.Item.Filename, err)
		}
		addUsage(&editionResult.Usage, usage)
		if selection.Selected {
			selected = append(selected, selectedItem{
				Item:       candidate.Item,
				Enrichment: candidate.Enrichment,
				Selection:  selection,
			})
		}
	}
	editionResult.Selected = len(selected)
	editionResult.Empty = len(selected) == 0

	usage, err := r.writeDigest(ctx, day, id, profile, selected, r.editionSources(edition, collected))
	addUsage(&editionResult.Usage, usage)
	if err != nil {
		return editionResult, err
	}
	return editionResult, nil
}

// admittedBySources narrows the enriched pool to what an edition's source list
// allows. An edition with no list draws on the whole pool; one with an empty
// list draws on nothing, and the two are opposites rather than degrees.
func admittedBySources(enriched []enrichedItem, edition config.Edition) []enrichedItem {
	if edition.SelectsAll() {
		return enriched
	}

	allowed := make(map[string]bool, len(edition.SourceIDs()))
	for _, id := range edition.SourceIDs() {
		allowed[id] = true
	}

	admitted := make([]enrichedItem, 0, len(enriched))
	for _, e := range enriched {
		if allowed[e.Item.Source] {
			admitted = append(admitted, e)
		}
	}
	return admitted
}

// loadCollectedOutcomes reads the day's collection record, returning nil when
// there is none.
//
// Absent is not an error: a day collected by an older build has no such file,
// and an aggregation that refused to run over it would be trading a digest for
// telemetry. A digest whose Sources is empty says "not known", which is honest,
// where a fabricated all-clear would not be.
func (r *Runner) loadCollectedOutcomes(day time.Time) map[string]collect.SourceOutcome {
	raw, err := os.ReadFile(r.store.CollectedPath(day)) //nolint:gosec // path is inside the engine's own data root
	if err != nil {
		return nil
	}

	var outcomes collect.Outcomes
	if err := json.Unmarshal(raw, &outcomes); err != nil {
		r.log.Warn("collection outcomes unreadable; digests will not carry them", "error", err)
		return nil
	}
	return outcomes.Sources
}

// editionSources narrows the day's collection outcomes to the sources one
// edition actually drew on.
//
// An edition reports only its own sources on purpose. A boardgames newsletter
// naming an unrelated feed's failure would be reporting on somebody else's
// pipeline, and the reason §8 wants outcomes at all is that a NARROW edition
// cannot otherwise tell a total upstream failure from a quiet day.
func (r *Runner) editionSources(edition config.Edition, collected map[string]collect.SourceOutcome) map[string]collect.SourceOutcome {
	if len(collected) == 0 {
		return nil
	}

	if edition.SelectsAll() {
		// Every configured source, not every recorded one: a source dropped
		// from the config mid-day is no longer part of this edition's pool.
		return pickSources(collected, sortedSourceIDs(r.cfg.Sources))
	}
	return pickSources(collected, edition.SourceIDs())
}

func pickSources(collected map[string]collect.SourceOutcome, ids []string) map[string]collect.SourceOutcome {
	picked := make(map[string]collect.SourceOutcome, len(ids))
	for _, id := range ids {
		if outcome, ok := collected[id]; ok {
			picked[id] = outcome
		}
	}
	if len(picked) == 0 {
		return nil
	}
	return picked
}

func sortedSourceIDs(sources map[string]config.Source) []string {
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// silentSourcesNote is the markdown half of ADR-0005 §8: it names the selected
// sources that produced nothing today, so an empty digest cannot be mistaken for
// a quiet day when the real answer is that the pipeline was down.
//
// 🚨 It never carries the error text, and that is not an oversight to be tidied
// away later. This markdown is what a public newsletter delivers, and a
// newsletter must not end with a fetch exception naming an internal host or
// path. The structured half carries the error for sinks and tooling; the two
// files are split by audience, which is the older rule this follows.
func silentSourcesNote(sources map[string]collect.SourceOutcome) string {
	var silent []string
	for id, outcome := range sources {
		if outcome.Items == 0 {
			silent = append(silent, id)
		}
	}
	if len(silent) == 0 {
		return ""
	}
	sort.Strings(silent)

	noun := "source"
	if len(silent) > 1 {
		noun = "sources"
	}
	return fmt.Sprintf("\nNo items were collected today from %d selected %s: %s.\n",
		len(silent), noun, strings.Join(silent, ", "))
}

// sortedEditionIDs gives editions a stable order, so two runs over the same day
// write the same things in the same sequence and a log is comparable.
func sortedEditionIDs(editions map[string]config.Edition) []string {
	ids := make([]string, 0, len(editions))
	for id := range editions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func addUsage(total *llm.Usage, add llm.Usage) {
	total.PromptTokens += add.PromptTokens
	total.CompletionTokens += add.CompletionTokens
	total.TotalTokens += add.TotalTokens
}

// readProfile reads a reader-owned relevance profile verbatim. The engine never
// infers, rewrites or caches an interpretation of one: it is the input the
// reader fully controls (ADR-0001).
func readProfile(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path comes from the operator's own config
	if err != nil {
		return "", fmt.Errorf("read relevance profile %s: %w", path, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		// An empty profile matches nothing, so every item would be passed over
		// and the edition's digest would always be empty — a silent, confusing
		// no-op. Say so.
		return "", fmt.Errorf("relevance profile %s is empty: it is what this edition selects against, so nothing would ever be selected", path)
	}
	return string(raw), nil
}

// enrichOutcome carries a neutral reading with the accounting for the call that
// produced it, so a reused enrichment can be told apart from a paid-for one.
type enrichOutcome struct {
	enrichment Enrichment
	usage      llm.Usage
}

// enrich returns one item's neutral reading, reusing a recorded one when the
// item has already been enriched. The second result reports whether it came
// from state.
//
// The cache key is the item filename alone, which is only correct because the
// result carries no audience: the same reading serves every edition. What it is
// NOT keyed on is what produced it, so the model and prompt version are checked
// before a record is trusted — a changed prompt would otherwise be served stale
// results indefinitely, with nothing in the config looking different.
func (r *Runner) enrich(ctx context.Context, item store.StoredItem, state State) (enrichOutcome, bool, error) {
	if prior, ok := state.Items[item.Filename]; ok && prior.Enrichment != nil &&
		prior.Status != StatusFailed &&
		prior.Model == r.cfg.Aggregator.Models.Item &&
		prior.PromptVersion == enrichPromptVersion {
		return enrichOutcome{enrichment: *prior.Enrichment, usage: prior.Usage}, true, nil
	}

	resp, err := r.client.Complete(ctx, llm.Request{
		Model:    r.cfg.Aggregator.Models.Item,
		Messages: buildEnrichMessages(item),
	})
	if err != nil {
		return enrichOutcome{}, false, err
	}

	enrichment, err := parseEnrichment(resp.Content)
	if err != nil {
		return enrichOutcome{}, false, err
	}
	return enrichOutcome{enrichment: enrichment, usage: resp.Usage}, false, nil
}

// selectItem asks one edition whether it wants an enriched item. It is given
// the neutral summary and data points rather than the full item, which is what
// makes a selection pass cheaper than the judgement it replaces.
func (r *Runner) selectItem(ctx context.Context, profile string, candidate enrichedItem) (Selection, llm.Usage, error) {
	resp, err := r.client.Complete(ctx, llm.Request{
		Model:    r.cfg.Aggregator.Models.Item,
		Messages: buildSelectMessages(profile, candidate),
	})
	if err != nil {
		return Selection{}, llm.Usage{}, err
	}

	selection, err := parseSelection(resp.Content)
	if err != nil {
		return Selection{}, resp.Usage, err
	}
	return selection, resp.Usage, nil
}

// writeDigest renders and writes both digest files, returning the markdown.
//
// A day with nothing relevant skips the model entirely: there is nothing to
// write a digest from, the correct output is the empty marker, and asking a
// model to write about nothing is exactly how filler gets produced.
func (r *Runner) writeDigest(ctx context.Context, day time.Time, edition, profile string, selected []selectedItem, sources map[string]collect.SourceOutcome) (llm.Usage, error) {
	var markdown string
	var usage llm.Usage

	if len(selected) == 0 {
		markdown = fmt.Sprintf("# Digest — %s\n\n%s%s\n", store.Day(day), emptyDigestMarker, silentSourcesNote(sources))
	} else {
		resp, err := r.client.Complete(ctx, llm.Request{
			Model:    r.cfg.Aggregator.Models.Digest,
			Messages: buildDigestMessages(profile, selected),
		})
		if err != nil {
			return usage, fmt.Errorf("digest pass: %w", err)
		}
		usage = resp.Usage
		body := strings.TrimSpace(resp.Content)
		if body == "" {
			return usage, errors.New("digest pass returned no text")
		}
		markdown = fmt.Sprintf("# Digest — %s\n\n%s\n", store.Day(day), body)
	}

	digest := Digest{
		Schema:      DigestSchema,
		Day:         store.Day(day),
		Edition:     edition,
		GeneratedAt: r.now().UTC().Format(time.RFC3339),
		Empty:       len(selected) == 0,
		Sources:     sources,
		Items:       make([]DigestItem, 0, len(selected)),
	}
	for _, s := range selected {
		digest.Items = append(digest.Items, DigestItem{
			Source:   s.Item.Source,
			URL:      s.Item.URL,
			Title:    s.Item.Title,
			Score:    s.Selection.Score,
			Reason:   s.Selection.Reason,
			Points:   s.Enrichment.Points,
			Tags:     s.Enrichment.Tags,
			Category: s.Enrichment.Category,
		})
	}

	structured, err := json.MarshalIndent(digest, "", "  ")
	if err != nil {
		return usage, fmt.Errorf("encode digest: %w", err)
	}

	mdPath, jsonPath := r.store.DigestPaths(day, edition)
	if err := r.store.WriteAtomic(mdPath, []byte(markdown)); err != nil {
		return usage, fmt.Errorf("write digest markdown: %w", err)
	}
	if err := r.store.WriteAtomic(jsonPath, append(structured, '\n')); err != nil {
		return usage, fmt.Errorf("write digest json: %w", err)
	}
	return usage, nil
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
	if loaded.Schema != stateSchema {
		// Relabelling another version's data as current is worse than losing
		// it: the fields may mean something different, and the cost of starting
		// fresh is one day's re-run rather than a digest built on
		// misinterpreted bookkeeping.
		r.log.Warn("state file was written by a different schema, starting the day fresh",
			"found", loaded.Schema, "want", stateSchema)
		return state
	}
	if loaded.Items == nil {
		loaded.Items = map[string]ItemState{}
	}
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

// unfence strips a fenced code block wrapper.
//
// Models routinely wrap JSON in one despite being asked not to, so it is
// removed before decoding rather than treated as a failure — the alternative is
// discarding a correct answer over its packaging.
func unfence(content string) string {
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
	return text
}

// parseEnrichment decodes the enrichment pass's reply.
func parseEnrichment(content string) (Enrichment, error) {
	text := unfence(content)

	var e Enrichment
	if err := json.Unmarshal([]byte(text), &e); err != nil {
		return Enrichment{}, fmt.Errorf("enrich pass did not return usable JSON: %w (got: %s)", err, snippet(text))
	}
	if strings.TrimSpace(e.Summary) == "" {
		// The summary is what every selection pass reads instead of the item.
		// Without it an edition would be judging blank text and would suppress
		// everything, which looks exactly like a quiet day.
		return Enrichment{}, errors.New("enrich pass returned no summary: it is what every edition reads in place of the item")
	}
	if e.Salience < 0 || e.Salience > 1 {
		// Out of range means the model was not working on the scale the floor
		// and the report are stated in, so the number is not comparable to
		// anything. Rejecting is better than clamping, which would invent a
		// value and hide that the scale was misunderstood.
		return Enrichment{}, fmt.Errorf("enrich pass returned salience %v, outside the 0.0-1.0 scale", e.Salience)
	}
	return e, nil
}

// parseSelection decodes one edition's verdict on one item.
func parseSelection(content string) (Selection, error) {
	text := unfence(content)

	var s Selection
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		return Selection{}, fmt.Errorf("select pass did not return usable JSON: %w (got: %s)", err, snippet(text))
	}
	return s, nil
}

func snippet(s string) string {
	const limit = 200
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}
