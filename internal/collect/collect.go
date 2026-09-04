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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

// cadenceDays is how many UTC days must pass before a source is due again.
// Monthly is 30 days rather than calendar-month arithmetic: for a fetch
// schedule, "every 30 days" is predictable and "the 31st of the month" is a bug
// report waiting to happen.
var cadenceDays = map[config.Cadence]int{
	config.CadenceDaily:   1,
	config.CadenceWeekly:  7,
	config.CadenceMonthly: 30,
}

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
type Collector interface {
	Collect(ctx context.Context, src config.Source) ([]Collected, error)
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
	return r
}

// SourceResult is what happened to one source in a pass.
type SourceResult struct {
	ID      string
	Skipped bool // not due under its cadence
	Written int
	Err     error
}

// Result is the outcome of a whole pass.
type Result struct {
	Sources []SourceResult

	// InboxDrained is how many files were taken out of the inbox and turned
	// into items.
	InboxDrained int

	// InboxErr is a failure of the drain itself, not of an individual file —
	// per-file failures are logged and skipped so one bad drop cannot block the
	// queue.
	InboxErr error
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

	drained, err := r.drainInbox(now)
	result.InboxDrained = drained
	result.InboxErr = err
	if err != nil {
		r.log.Error("inbox drain failed", "error", err)
	}

	for _, id := range sortedSourceIDs(r.cfg.Sources) {
		src := r.cfg.Sources[id]

		due, err := r.due(id, src.Cadence, now)
		if err != nil {
			result.Sources = append(result.Sources, SourceResult{ID: id, Err: err})
			r.log.Error("cadence check failed", "source", id, "error", err)
			continue
		}
		if !due {
			result.Sources = append(result.Sources, SourceResult{ID: id, Skipped: true})
			r.log.Debug("source not due", "source", id, "cadence", string(src.Cadence))
			continue
		}

		written, err := r.collectSource(ctx, id, src, now)
		result.Sources = append(result.Sources, SourceResult{ID: id, Written: written, Err: err})
		if err != nil {
			r.log.Error("source failed", "source", id, "written", written, "error", err)
			continue
		}
		r.log.Info("source collected", "source", id, "items", written)
	}

	return result
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

	items, err := collector.Collect(ctx, src)
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
	days, ok := cadenceDays[cadence]
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
