// Package deliver is layer 3: dumb by design.
//
// A digest goes in, a delivery goes out. There is no intelligence here and none
// should be added — deciding what the reader sees is the aggregator's job, and
// a sink that filtered or reworded a digest would be quietly overruling it.
package deliver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/store"
)

// defaultTimeout bounds one delivery. ADR-0003 §5 wants this configurable per
// sink; the config has no field for it yet, so the constant stands in and every
// sink shares it. A hung delivery must not hold the run.
const defaultTimeout = 60 * time.Second

// Digest is what a sink receives: the day's digest in both forms, so a delivery
// can send prose or work from the structured items without re-reading the disk.
type Digest struct {
	Day string

	// Edition names the audience this digest was written for, so a sink and its
	// logs can say which of several they carried. It is empty for the daily
	// report, which is not an edition.
	Edition string

	// IsReport says this payload is the daily operator report rather than an
	// edition's digest. A sink is told rather than left to infer it from an
	// empty Edition, because "no edition" and "the report" would otherwise be
	// the same reading — the exact conflation the config binding refuses.
	IsReport bool

	// Markdown is the human-readable artifact.
	Markdown string

	// Structured is the raw `digests/<edition>/<day>.json` bytes, passed through
	// unmodified so an exec sink sees exactly what is on disk.
	Structured []byte

	// Empty says the day produced nothing above the relevance bar. Sinks are
	// told rather than left to infer it from an absent field.
	Empty bool
}

// Sink delivers one digest. Implementations do exactly that and nothing else.
type Sink interface {
	Deliver(ctx context.Context, digest Digest) error
}

// Runner delivers a day's digest to every configured sink.
type Runner struct {
	cfg   *config.Config
	store *store.Store
	log   *slog.Logger

	// build resolves a config entry to a Sink. Replaceable so a test can supply
	// a sink without a network or a filesystem.
	build func(id string, sink config.Sink) (Sink, error)
}

// Option customises a Runner.
type Option func(*Runner)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(r *Runner) { r.log = l } }

// WithSinkBuilder replaces sink construction.
func WithSinkBuilder(b func(id string, sink config.Sink) (Sink, error)) Option {
	return func(r *Runner) { r.build = b }
}

// NewRunner builds a Runner over a validated config.
func NewRunner(cfg *config.Config, opts ...Option) *Runner {
	r := &Runner{
		cfg:   cfg,
		store: store.New(cfg.DataRootPath()),
		log:   slog.Default(),
	}
	r.build = r.defaultBuild
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// SinkResult is one sink's outcome.
type SinkResult struct {
	ID string

	// Edition is the edition this sink carried, empty for a sink bound to the
	// daily report.
	Edition string

	// Report says this sink carried the daily report rather than an edition.
	Report bool

	// Empty says the digest this sink delivered was a quiet day. It lives here
	// rather than on the run because editions differ: one audience can have a
	// full digest on a day another has nothing, and a single run-level flag
	// would have to pick one of them to be wrong about.
	Empty bool

	Err error
}

// Result is the outcome of one delivery pass.
type Result struct {
	Day   string
	Sinks []SinkResult
}

// Failed reports whether any sink failed.
func (r Result) Failed() bool {
	for _, s := range r.Sinks {
		if s.Err != nil {
			return true
		}
	}
	return false
}

// Run delivers one day's digest to every configured sink.
//
// An empty digest is still delivered. Silence has to be distinguishable from
// breakage for the reader too, not only on disk: a day where the engine ran and
// found nothing is a result worth receiving, and suppressing it would make a
// quiet day and a broken pipeline look identical from the outside.
//
// One failing sink does not stop the others: a chat delivery being down is no
// reason for the file copy not to be written. That now extends to a missing
// digest, which is recorded against the sinks bound to that edition rather than
// returned as a run error — with several editions, one that was never
// aggregated must not stop the ones that were.
func (r *Runner) Run(ctx context.Context, day time.Time) (Result, error) {
	result := Result{Day: store.Day(day)}

	ids := make([]string, 0, len(r.cfg.Sinks))
	for id := range r.cfg.Sinks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	if len(ids) == 0 {
		r.log.Info("no sinks configured; the digests are on disk only", "day", result.Day)
		return result, nil
	}

	// Each sink names the edition it carries, so digests are read per edition
	// rather than once for the run. They are cached across sinks because many
	// sinks legitimately name one edition — that is how a single digest reaches
	// both a chat and a file — and re-reading the same pair of files per sink
	// would be work for nothing.
	digests := map[string]Digest{}

	for _, id := range ids {
		configured := r.cfg.Sinks[id]

		// The report is keyed under a name no edition can have, since an
		// edition id cannot contain a slash. Sharing one cache with the
		// editions is safe because of that, not by luck.
		const reportKey = "/report"

		edition, isEdition := configured.EditionID()
		key := edition
		if !isEdition {
			key = reportKey
		}

		digest, cached := digests[key]
		if !cached {
			var loaded Digest
			var err error
			if isEdition {
				loaded, err = r.readDigest(day, edition)
			} else {
				loaded, err = r.readReport(day)
			}
			if err != nil {
				result.Sinks = append(result.Sinks, SinkResult{ID: id, Edition: edition, Err: err})
				r.log.Error("cannot deliver", "sink", id, "edition", edition, "error", err)
				continue
			}
			digests[key] = loaded
			digest = loaded
		}

		sink, err := r.build(id, configured)
		if err != nil {
			result.Sinks = append(result.Sinks, SinkResult{ID: id, Edition: edition, Err: err})
			r.log.Error("sink could not be built", "sink", id, "error", err)
			continue
		}

		sinkCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
		err = sink.Deliver(sinkCtx, digest)
		cancel()

		result.Sinks = append(result.Sinks, SinkResult{
			ID: id, Edition: edition, Report: digest.IsReport, Empty: digest.Empty, Err: err,
		})
		if err != nil {
			r.log.Error("delivery failed", "sink", id, "edition", edition, "error", err)
			continue
		}
		r.log.Info("delivered", "sink", id, "edition", edition,
			"report", digest.IsReport, "day", result.Day, "empty", digest.Empty)
	}

	return result, nil
}

// readReport loads both of the day's report files.
//
// An absent report means the aggregator has not run. Delivering nothing in its
// place would tell the operator their telemetry is empty when in fact it was
// never written.
//
// Unlike a digest, that sentence needs no qualifying: reports/ was introduced
// after ADR-0005 nested the digests, so a report has only ever had one path and
// there is no older location a file could be hiding at.
func (r *Runner) readReport(day time.Time) (Digest, error) {
	mdPath, jsonPath := r.store.ReportPaths(day)

	markdown, err := os.ReadFile(mdPath) //nolint:gosec // path is inside the engine's own data root
	if err != nil {
		if os.IsNotExist(err) {
			return Digest{}, fmt.Errorf("no report for %s: the aggregator has not run for that day", store.Day(day))
		}
		return Digest{}, fmt.Errorf("read report: %w", err)
	}

	structured, err := os.ReadFile(jsonPath) //nolint:gosec // path is inside the engine's own data root
	if err != nil {
		return Digest{}, fmt.Errorf("read structured report: %w", err)
	}

	return Digest{
		Day:        store.Day(day),
		IsReport:   true,
		Markdown:   string(markdown),
		Structured: structured,
	}, nil
}

// readDigest loads both of one edition's digest files for a day.
func (r *Runner) readDigest(day time.Time, edition string) (Digest, error) {
	mdPath, jsonPath := r.store.DigestPaths(day, edition)

	markdown, err := os.ReadFile(mdPath) //nolint:gosec // path is inside the engine's own data root
	if err != nil {
		if os.IsNotExist(err) {
			// Absent is not empty: ADR-0002 makes a quiet edition write files
			// with an explicit marker, so nothing here means no digest was
			// written. Delivering an invented empty digest would erase that.
			return Digest{}, r.absentDigestError(day, edition, mdPath)
		}
		return Digest{}, fmt.Errorf("read digest: %w", err)
	}

	structured, err := os.ReadFile(jsonPath) //nolint:gosec // path is inside the engine's own data root
	if err != nil {
		return Digest{}, fmt.Errorf("read structured digest: %w", err)
	}

	empty, err := digestIsEmpty(structured)
	if err != nil {
		return Digest{}, err
	}

	return Digest{
		Day:        store.Day(day),
		Edition:    edition,
		Markdown:   string(markdown),
		Structured: structured,
		Empty:      empty,
	}, nil
}

// absentDigestError explains an absent digest as the thing it actually is.
//
// Two different situations leave nothing at the expected path, and they have
// different remedies. The aggregator may genuinely not have produced a digest
// for that day — a scheduling or crash question. Or it produced one before
// ADR-0005 moved digests under an edition directory, and the file is sitting at
// the flat path that ADR deliberately left populated.
//
// 🚨 Reporting the second as the first is worse than unhelpful: it asserts
// something false about a component that is healthy, and it does so to a reader
// who is already hunting. They go and read the aggregator's schedule and its
// logs, find nothing wrong, and are no closer, while the digest they are looking
// for is on disk the whole time.
//
// expected is the path already looked at, named so the reader can see which
// layout this engine believes in without having to reconstruct it.
func (r *Runner) absentDigestError(day time.Time, edition, expected string) error {
	legacyMarkdown, legacyStructured := r.store.LegacyDigestPaths(day)

	switch {
	case isFile(legacyMarkdown) && isFile(legacyStructured):
		// The pair is what licenses the sentence "it ran". A digest was always
		// written as both files, so finding both is evidence about the
		// aggregator rather than about whatever happens to sit in that
		// directory.
		//
		// The digest is not described as this edition's. It predates editions
		// existing, so it belongs to none of them, and naming one here would
		// trade a false claim about the aggregator for a false claim about the
		// file.
		return fmt.Errorf(
			"no digest for edition %s on %s at %s: the aggregator did run for that day and wrote %s, "+
				"the flat path digests used before they moved under digests/<edition>/ — the layout is stale, not the aggregator",
			edition, store.Day(day), expected, legacyMarkdown)

	case isFile(legacyMarkdown) || isFile(legacyStructured):
		// One file alone is not that evidence. A file sink resolves its path as
		// given, so a sink pointed at digests/{day}.md writes exactly this
		// shape — which is why ADR-0005 moves the example config off that tree
		// and warns against it. Name the file and leave the reader to look
		// rather than attributing it to the aggregator.
		found := legacyMarkdown
		if !isFile(found) {
			found = legacyStructured
		}
		return fmt.Errorf(
			"no digest for edition %s on %s: the aggregator has not run for that day "+
				"(%s exists, but a digest is a .md/.json pair, so that file on its own is not one)",
			edition, store.Day(day), found)

	default:
		return fmt.Errorf("no digest for edition %s on %s: the aggregator has not run for that day", edition, store.Day(day))
	}
}

// isFile says whether a path is present as a regular file. Anything else —
// absent, a directory, an unreadable parent — is not the digest being looked
// for, and this runs only on a path that already failed to open, so a stat
// error has nothing left to report.
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// defaultBuild resolves a configured sink to a built-in or an external command.
func (r *Runner) defaultBuild(id string, sink config.Sink) (Sink, error) {
	if len(sink.Command) > 0 {
		return &execSink{id: id, command: sink.Command, params: sink.Params, env: sink.Env}, nil
	}

	switch sink.Type {
	case "file":
		return newFileSink(sink)
	case "telegram":
		return newTelegramSink(sink)
	case "":
		return nil, errors.New("sink has neither type nor command")
	default:
		return nil, fmt.Errorf("unknown sink type %q, want one of %s, or set command for an external sink", sink.Type, builtinSinkList)
	}
}

// builtinSinkList names the built-in deliveries for error messages. Kept next
// to the switch above so the two cannot drift.
const builtinSinkList = "file, telegram"
