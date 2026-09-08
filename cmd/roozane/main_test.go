package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/roozane/internal/aggregate"
)

func TestRunVersion(t *testing.T) {
	// Stamp a sentinel rather than asserting against `version` itself: comparing
	// the output to the same variable it is printed from would pass even if the
	// subcommand printed a hard-coded constant, so the assertion has to name a
	// value only the stamp can produce.
	original := version
	version = "v0.0.0-test"
	t.Cleanup(func() { version = original })

	var stdout, stderr bytes.Buffer

	code := run([]string{"version"}, &stdout, &stderr)

	require.Equal(t, 0, code)
	assert.Equal(t, "v0.0.0-test\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestRunUnknownArgs(t *testing.T) {
	for name, args := range map[string][]string{
		"no arguments": {},
		// A word chosen not to become a real subcommand: this fixture has been
		// invalidated three times by the command it named getting implemented.
		"unknown subcommand": {"definitely-not-a-command"},
		"trailing argument":  {"version", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(args, &stdout, &stderr)

			assert.Equal(t, 2, code)
			assert.Empty(t, stdout.String(), "usage goes to stderr, so stdout stays pipeable")
			assert.Contains(t, stderr.String(), "usage:")
		})
	}
}

// TestAggregateExitCodeReportsUnjudgedItems is the guard on the half of #64
// that is easiest to lose. Tolerating a failed select removed the error that
// used to make the run exit non-zero, so this clause is the only thing left
// telling a scheduler that part of the day was never assessed.
func TestAggregateExitCodeReportsUnjudgedItems(t *testing.T) {
	for name, tc := range map[string]struct {
		result aggregate.Result
		want   int
	}{
		"a clean run": {
			result: aggregate.Result{
				Items: 3, Enriched: 3,
				Editions: []aggregate.EditionResult{{ID: "default", Candidates: 3, Selected: 2}},
			},
			want: 0,
		},
		"an enrichment failure": {
			result: aggregate.Result{Items: 3, Enriched: 2, Failed: 1},
			want:   1,
		},
		// The case the fix introduces: enrichment is clean, the edition wrote
		// its digest, and items were still never judged. Every number this run
		// reports other than Unjudged reads as a success.
		"a selection failure with nothing else wrong": {
			result: aggregate.Result{
				Items: 3, Enriched: 3, Failed: 0,
				Editions: []aggregate.EditionResult{{ID: "default", Candidates: 3, Selected: 2, Unjudged: 1}},
			},
			want: 1,
		},
		"a selection failure in the second of two editions": {
			result: aggregate.Result{
				Items: 3, Enriched: 3,
				Editions: []aggregate.EditionResult{
					{ID: "a", Candidates: 3, Selected: 3},
					{ID: "b", Candidates: 3, Selected: 1, Unjudged: 2},
				},
			},
			want: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, aggregateExitCode(tc.result))
		})
	}
}
