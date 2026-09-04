// Command roozane is the single binary that runs the pipeline described in
// ADR-0001.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/yaad-index/roozane/internal/aggregate"
	"github.com/yaad-index/roozane/internal/collect"
	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/store"
)

// version is overwritten at link time with the release version:
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/roozane
//
// An unstamped build reports "dev", which is honest about being one rather than
// claiming a release it is not.
var version = "dev"

const usage = `usage:
  roozane version
  roozane collect   [-config roozane.yaml] [-v]
  roozane aggregate [-config roozane.yaml] [-day YYYY-MM-DD] [-v]
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable body of main: it takes its arguments and streams rather
// than reading globals, and returns the process exit code instead of calling
// os.Exit, so the behaviour below can be asserted directly.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintf(stderr, "roozane %s\n\n%s", version, usage)
		return 2
	}

	switch args[0] {
	case "version":
		if len(args) > 1 {
			_, _ = fmt.Fprintf(stderr, "version takes no arguments\n\n%s", usage)
			return 2
		}
		// Writes to the process's own stdout: there is nowhere useful to report
		// a failed write to, and the exit code already carries the outcome.
		_, _ = fmt.Fprintln(stdout, version)
		return 0

	case "collect":
		return runCollect(args[1:], stdout, stderr)

	case "aggregate":
		return runAggregate(args[1:], stdout, stderr)

	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// runCollect performs one collection pass and prints a per-source summary.
func runCollect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "roozane.yaml", "path to the configuration file")
	verbose := fs.Bool("v", false, "log every skipped source and per-item decision")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	result := collect.NewRunner(cfg, collect.WithLogger(logger)).Run(context.Background())

	// The summary goes to stdout so it can be piped, while the log goes to
	// stderr — a cron job can keep one and discard the other.
	if result.InboxDrained > 0 {
		_, _ = fmt.Fprintf(stdout, "inbox: %d drained\n", result.InboxDrained)
	}
	for _, s := range result.Sources {
		switch {
		case s.Err != nil:
			_, _ = fmt.Fprintf(stdout, "%s: FAILED (%d written): %v\n", s.ID, s.Written, s.Err)
		case s.Skipped:
			_, _ = fmt.Fprintf(stdout, "%s: not due\n", s.ID)
		default:
			_, _ = fmt.Fprintf(stdout, "%s: %d items\n", s.ID, s.Written)
		}
	}

	// A failed pass keeps whatever it did collect — ADR-0003 has no
	// partial-success protocol — but the exit code still reports the failure so
	// a scheduler notices.
	if result.Failed() {
		return 1
	}
	return 0
}

// runAggregate judges one day's collected items and writes its digest.
func runAggregate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aggregate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "roozane.yaml", "path to the configuration file")
	dayFlag := fs.String("day", "", "UTC day to aggregate (YYYY-MM-DD); defaults to today")
	verbose := fs.Bool("v", false, "log per-item decisions")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	day := time.Now().UTC()
	if *dayFlag != "" {
		parsed, err := time.Parse(store.DayFormat, *dayFlag)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "invalid -day %q: want YYYY-MM-DD\n", *dayFlag)
			return 2
		}
		day = parsed
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	runner, err := aggregate.NewRunner(cfg, aggregate.WithLogger(logger))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	result, err := runner.Run(context.Background(), day)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "%s: %d items, %d relevant, %d suppressed, %d failed, %d reused\n",
		result.Day, result.Items, result.Relevant, result.Suppressed, result.Failed, result.Reused)
	if result.Empty {
		_, _ = fmt.Fprintln(stdout, "digest: empty (nothing cleared the relevance bar)")
	}
	_, _ = fmt.Fprintf(stdout, "tokens: %d prompt, %d completion\n",
		result.Usage.PromptTokens, result.Usage.CompletionTokens)

	// A failed item does not invalidate the digest that was written, but the
	// exit code still reports it so a scheduler notices.
	if result.Failed > 0 {
		return 1
	}
	return 0
}
