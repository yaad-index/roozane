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

	// Markdown is the human-readable artifact.
	Markdown string

	// Structured is the raw `digests/<day>.json` bytes, passed through
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
	ID  string
	Err error
}

// Result is the outcome of one delivery pass.
type Result struct {
	Day   string
	Empty bool
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
// reason for the file copy not to be written.
func (r *Runner) Run(ctx context.Context, day time.Time) (Result, error) {
	result := Result{Day: store.Day(day)}

	digest, err := r.readDigest(day)
	if err != nil {
		return result, err
	}
	result.Empty = digest.Empty

	ids := make([]string, 0, len(r.cfg.Sinks))
	for id := range r.cfg.Sinks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	if len(ids) == 0 {
		r.log.Info("no sinks configured; the digest is on disk only", "day", result.Day)
		return result, nil
	}

	for _, id := range ids {
		sink, err := r.build(id, r.cfg.Sinks[id])
		if err != nil {
			result.Sinks = append(result.Sinks, SinkResult{ID: id, Err: err})
			r.log.Error("sink could not be built", "sink", id, "error", err)
			continue
		}

		sinkCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
		err = sink.Deliver(sinkCtx, digest)
		cancel()

		result.Sinks = append(result.Sinks, SinkResult{ID: id, Err: err})
		if err != nil {
			r.log.Error("delivery failed", "sink", id, "error", err)
			continue
		}
		r.log.Info("delivered", "sink", id, "day", result.Day, "empty", digest.Empty)
	}

	return result, nil
}

// readDigest loads both digest files for a day.
func (r *Runner) readDigest(day time.Time) (Digest, error) {
	mdPath, jsonPath := r.store.DigestPaths(day, config.DefaultEdition)

	markdown, err := os.ReadFile(mdPath) //nolint:gosec // path is inside the engine's own data root
	if err != nil {
		if os.IsNotExist(err) {
			// Absent is not empty: ADR-0002 makes a quiet day write files with
			// an explicit marker, so nothing here means the aggregator has not
			// run. Delivering an invented empty digest would erase that.
			return Digest{}, fmt.Errorf("no digest for %s: the aggregator has not run for that day", store.Day(day))
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
		Markdown:   string(markdown),
		Structured: structured,
		Empty:      empty,
	}, nil
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
