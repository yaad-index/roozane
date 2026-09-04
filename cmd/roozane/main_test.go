package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		"no arguments":       {},
		"unknown subcommand": {"collect"},
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
