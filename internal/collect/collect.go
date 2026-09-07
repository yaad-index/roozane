// Package collect is layer 1: the dumb one.
//
// It fetches what the configuration points at and hands the raw text to the
// store. There is no AI here, no ranking, no filtering and no summarising —
// ADR-0001 puts all of that in the aggregator, and the value of keeping this
// layer stupid is that a collector can never quietly decide something is not
// worth the reader's attention.
package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/store"
)

// maxItemBytes caps a single item's text. The whole stream passes through the
// engine, so an unbounded page is an unbounded allocation. ADR-0003 makes this
// a config knob for plugins with the same 1 MiB default; the built-ins use the
// constant until the plugin runner introduces the knob, so the two cannot
// disagree in the meantime.
const maxItemBytes = 1 << 20

// truncationMarker is appended to an item that hit the cap, so a reader can see
// that the text is short because it was cut rather than because the source was.
const truncationMarker = "\n\n[roozane: item truncated at the size cap]\n"

// defaultFetchTimeout bounds a single fetch. A source that hangs must not hold
// the day's run hostage.
const defaultFetchTimeout = 30 * time.Second

// Collected is one item as a collector produces it. It carries no identity and
// no timestamp of the engine's: ADR-0003 reserves those for the engine so that
// nothing a collector returns can choose which day folder it lands in.
type Collected struct {
	// URL is the item's address when it has one. It is the identity the
	// filename is keyed on, so a stable URL means a stable filename.
	URL string

	// Title is the source's own title for the item, if any.
	Title string

	// SourceTime is a timestamp the item claimed for itself — a feed's
	// publication date. Provenance only.
	SourceTime time.Time

	// Content is the raw text, unmodified.
	Content string
}

// Collector fetches the items currently available from one configured source.
// Implementations are deliberately thin: fetch, extract text, return.
//
// The source id is passed alongside the source because ADR-0003 §2 puts it in
// the envelope an exec plugin receives. The built-in collectors ignore it.
type Collector interface {
	Collect(ctx context.Context, id string, src config.Source) ([]Collected, error)
}

// Runner performs one collection pass over a configuration.
type Runner struct {
	cfg        *config.Config
	store      *store.Store
	collectors map[string]Collector
	log        *slog.Logger
	now        func() time.Time
}

// Option customises a Runner. The defaults are what production uses; these
// exist so tests can supply a clock and an HTTP client without a live network.
type Option func(*Runner)

// WithClock replaces the wall clock. The engine stamps every item's fetched_at
// from it, so a test can pin the day folder it writes into.
func WithClock(now func() time.Time) Option {
	return func(r *Runner) { r.now = now }
}

// WithCollector overrides or adds a collector for a type name.
func WithCollector(name string, c Collector) Option {
	return func(r *Runner) { r.collectors[name] = c }
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(r *Runner) { r.log = l }
}

// NewRunner builds a Runner over a validated config.
func NewRunner(cfg *config.Config, opts ...Option) *Runner {
	client := &http.Client{Timeout: defaultFetchTimeout}

	r := &Runner{
		cfg:   cfg,
		store: store.New(cfg.DataRootPath()),
		collectors: map[string]Collector{
			"feed": &feedCollector{client: client},
			"http": &httpCollector{client: client},
		},
		log: slog.Default(),
		now: time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}

	// Registered after the options because it needs the Runner's logger, which
	// an option may have replaced — and skipped when a caller supplied its own
	// exec collector, so WithCollector still wins.
	if _, ok := r.collectors["exec"]; !ok {
		r.collectors["exec"] = &execCollector{log: r.log}
	}
	return r
}

// SourceResult is what happened to one source in a pass.
type SourceResult struct {
	ID      string
	Skipped bool // not due under its cadence
	Written int
	Err     error
}

// outcomesSchema versions days/<day>/collected.json. It carries one from the
// start for the same reason the digest does: something other than this package
// reads it.
const outcomesSchema = 1

// SourceOutcome is one source's line in the day's collection record.
type SourceOutcome struct {
	// Ran says the collector was invoked, which is false for a source that was
	// not due. It is not the same as succeeding — a source that ran and failed
	// has Ran true and an Error.
	Ran bool `json:"ran"`

	// Items is how many items reached disk, which can be non-zero alongside an
	// error: a partial success is visible rather than rounded down.
	Items int `json:"items"`

	// Error is the failure text, empty when there was none.
	Error string `json:"error,omitempty"`
}

// Outcomes is `days/<day>/collected.json` (ADR-0005 §6): what each source did
// today, written for humans and for the daily report.
//
// 🚨 This exists because the information genuinely does not exist anywhere else.
// ADR-0004 §1 decides deliberately that a failed fetch writes no ran/ marker, so
// a failed source stays indistinguishable from one that was never due and is
// retried on the next pass. That ambiguity is load-bearing for due(), not an
// oversight — which is exactly why ran/ cannot answer "what failed", and why
// this file has to.
//
// ⚠️ due() must never read this file. The moment scheduling consults it,
// ADR-0004's retry property changes meaning silently: a source that failed would
// start looking like one that had run, and stop being retried. The invariant is
// held by a test rather than only by this comment, because the file looks useful
// to due() and the damage would be invisible.
type Outcomes struct {
	Schema    int                      `json:"schema"`
	Day       string                   `json:"day"`
	UpdatedAt string                   `json:"updated_at"`
	Sources   map[string]SourceOutcome `json:"sources"`
}

// Result is the outcome of a whole pass.
type Result struct {
	Sources []SourceResult

	// InboxDrained is how many files were taken out of the inbox and turned
	// into items.
	InboxDrained int

	// Pruned is what the pass removed to honour the retention windows.
	Pruned store.PruneResult

	// PruneErr is a failure to enforce retention. It is reported beside the
	// collection rather than folded into it: old data that would not delete
	// does not make what was collected wrong.
	PruneErr error

	// InboxErr is a failure of the drain itself, not of an individual file —
	// per-file failures are logged and skipped so one bad drop cannot block the
	// queue.
	InboxErr error

	// OutcomesErr is a failure to record the day's collection outcomes. It is
	// reported beside the collection rather than folded into it, on the same
	// reasoning as PruneErr: telemetry that would not write does not make what
	// was collected wrong, and refusing the pass over it would be the wrong
	// trade.
	OutcomesErr error
}

// Failed reports whether anything in the pass failed. A pass with failures
// still keeps everything it did collect: ADR-0003 has no partial-success
// protocol, and items that parsed were valid when they parsed.
func (r Result) Failed() bool {
	if r.InboxErr != nil {
		return true
	}
	for _, s := range r.Sources {
		if s.Err != nil {
			return true
		}
	}
	return false
}

// Run performs one collection pass: drain the inbox, then collect from every
// source that is due.
//
// A failing source does not stop the others. The alternative — aborting the
// pass — would let one broken feed cost a reader their whole digest, which is
// the opposite of what a news engine is for.
func (r *Runner) Run(ctx context.Context) Result {
	now := r.now().UTC()
	var result Result

	// Housekeeping before work: retention is enforced against the layout the
	// cadence checks below are about to read, so a pass never decides due-ness
	// from folders it is about to delete.
	pruned, err := r.store.Prune(r.cfg.Retention.ItemDays(), r.cfg.Retention.Digests, now)
	result.Pruned = pruned
	if err != nil {
		// Retention failing does not make the collection wrong, and refusing to
		// collect because old data would not delete is the wrong trade.
		result.PruneErr = err
		r.log.Error("retention prune failed", "error", err)
	}
	if pruned.Days > 0 || pruned.Digests > 0 {
		r.log.Info("pruned past the retention window", "days", pruned.Days, "digests", pruned.Digests)
	}

	drained, err := r.drainInbox(now)
	result.InboxDrained = drained
	result.InboxErr = err
	if err != nil {
		r.log.Error("inbox drain failed", "error", err)
	}

	// The day's record so far. A pass merges into it rather than replacing it,
	// because collection runs more than once a day: a source that ran at one
	// slot is usually not due at the next, and rewriting the file from this
	// pass alone would erase what the earlier one recorded.
	outcomes := r.loadOutcomes(now)

	for _, id := range sortedSourceIDs(r.cfg.Sources) {
		src := r.cfg.Sources[id]

		due, err := r.due(id, src.Cadence, now)
		if err != nil {
			result.Sources = append(result.Sources, SourceResult{ID: id, Err: err})
			noteNotRun(outcomes, id, err)
			r.log.Error("cadence check failed", "source", id, "error", err)
			continue
		}
		if !due {
			result.Sources = append(result.Sources, SourceResult{ID: id, Skipped: true})
			noteNotRun(outcomes, id, nil)
			r.log.Debug("source not due", "source", id, "cadence", string(src.Cadence))
			continue
		}

		written, err := r.collectSource(ctx, id, src, now)
		result.Sources = append(result.Sources, SourceResult{ID: id, Written: written, Err: err})
		outcomes.Sources[id] = SourceOutcome{Ran: true, Items: written, Error: errorText(err)}
		if err != nil {
			// No marker on a failure: its absence has to keep meaning "not due
			// yet, or broken" so the source is retried rather than treated as
			// having legitimately found nothing (ADR-0004).
			r.log.Error("source failed", "source", id, "written", written, "error", err)
			continue
		}

		if written == 0 {
			// The fetch succeeded and there was nothing there. Without a marker
			// the layout would read as never-collected and re-fetch this source
			// on every pass regardless of its cadence.
			if err := r.store.MarkRan(id, now); err != nil {
				// A missing marker costs one duplicate fetch on the next pass,
				// which is not a collection failure — the items, of which there
				// are none, are not what went wrong.
				r.log.Error("could not record empty run", "source", id, "error", err)
			}
		}
		r.log.Info("source collected", "source", id, "items", written)
	}

	if err := r.saveOutcomes(now, outcomes); err != nil {
		result.OutcomesErr = err
		r.log.Error("could not record collection outcomes", "error", err)
	}

	return result
}

// noteNotRun records a source the collector did not invoke this pass.
//
// It never overwrites an existing entry: a source that ran earlier today and is
// simply not due now must keep the record of what it did, and a "not run" line
// written over it would turn a real collection into a blank.
func noteNotRun(outcomes Outcomes, id string, err error) {
	if _, recorded := outcomes.Sources[id]; recorded {
		return
	}
	outcomes.Sources[id] = SourceOutcome{Ran: false, Error: errorText(err)}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// loadOutcomes reads the day's record so far, returning an empty one when there
// is nothing usable. An unreadable or foreign-schema file is started over rather
// than merged into: this is telemetry, so the cost of losing it is a gap in a
// report, and the cost of merging into a shape that may mean something else is a
// report that misdescribes the day.
func (r *Runner) loadOutcomes(now time.Time) Outcomes {
	fresh := Outcomes{Schema: outcomesSchema, Day: store.Day(now), Sources: map[string]SourceOutcome{}}

	raw, err := os.ReadFile(r.store.CollectedPath(now)) //nolint:gosec // path is inside the engine's own data root
	if err != nil {
		return fresh
	}

	var loaded Outcomes
	if err := json.Unmarshal(raw, &loaded); err != nil {
		r.log.Warn("collection outcomes unreadable, starting the day's record fresh", "error", err)
		return fresh
	}
	if loaded.Schema != outcomesSchema {
		r.log.Warn("collection outcomes were written by a different schema, starting the day's record fresh",
			"found", loaded.Schema, "want", outcomesSchema)
		return fresh
	}
	if loaded.Sources == nil {
		loaded.Sources = map[string]SourceOutcome{}
	}
	loaded.Day = fresh.Day
	return loaded
}

func (r *Runner) saveOutcomes(now time.Time, outcomes Outcomes) error {
	outcomes.Schema = outcomesSchema
	outcomes.UpdatedAt = now.UTC().Format(time.RFC3339)

	raw, err := json.MarshalIndent(outcomes, "", "  ")
	if err != nil {
		return fmt.Errorf("encode collection outcomes: %w", err)
	}
	if err := r.store.WriteAtomic(r.store.CollectedPath(now), append(raw, '\n')); err != nil {
		return fmt.Errorf("write collection outcomes: %w", err)
	}
	return nil
}

// collectSource runs one source's collector and writes what it returns. It
// reports how many items reached disk even when it returns an error, so a
// partial success is visible rather than rounded down to zero.
func (r *Runner) collectSource(ctx context.Context, id string, src config.Source, now time.Time) (int, error) {
	collector, ok := r.collectors[src.Collector]
	if !ok {
		// Config validation rejects unknown collector types, so reaching this
		// means the registry and the allow-list have drifted apart.
		return 0, fmt.Errorf("no collector registered for type %q", src.Collector)
	}

	items, err := collector.Collect(ctx, id, src)
	if err != nil {
		return 0, fmt.Errorf("collect: %w", err)
	}

	var written int
	var problems []error
	for _, item := range items {
		if item.Content == "" {
			// An item with no text is not something the aggregator can read,
			// and writing it would put an empty file in the day folder that
			// looks like a collection that happened.
			r.log.Warn("skipping item with no content", "source", id, "url", item.URL)
			continue
		}

		content, truncated := capContent(item.Content)
		if truncated {
			r.log.Warn("item truncated at the size cap", "source", id, "url", item.URL, "cap_bytes", maxItemBytes)
		}

		if _, err := r.store.WriteItem(store.Item{
			Source:     id,
			URL:        item.URL,
			Title:      item.Title,
			FetchedAt:  now,
			SourceTime: item.SourceTime,
			Collector:  src.Collector,
			Content:    content,
		}); err != nil {
			problems = append(problems, fmt.Errorf("write item %q: %w", item.URL, err))
			continue
		}
		written++
	}

	return written, errors.Join(problems...)
}

// due reports whether a source should be collected now, from the layout alone.
func (r *Runner) due(id string, cadence config.Cadence, now time.Time) (bool, error) {
	days, ok := cadence.Days()
	if !ok {
		return false, fmt.Errorf("unknown cadence %q", cadence)
	}

	// Look back one period: anything older than that makes the source due, so
	// there is no reason to read further.
	last, found, err := r.store.SourceLastCollected(id, now, days)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}

	elapsed := daysBetween(last, now)
	return elapsed >= days, nil
}

// daysBetween counts whole UTC calendar days from a to b. Both are truncated to
// their day first, so "yesterday at 23:00" to "today at 01:00" is one day and
// not zero — the cadence is in days, not in elapsed hours.
func daysBetween(a, b time.Time) int {
	dayA := a.UTC().Truncate(24 * time.Hour)
	dayB := b.UTC().Truncate(24 * time.Hour)
	return int(dayB.Sub(dayA).Hours() / 24)
}

// capContent enforces maxItemBytes, reporting whether it had to.
func capContent(content string) (string, bool) {
	if len(content) <= maxItemBytes {
		return content, false
	}
	return content[:maxItemBytes] + truncationMarker, true
}

func sortedSourceIDs(sources map[string]config.Source) []string {
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
