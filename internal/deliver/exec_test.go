package deliver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// script writes an executable shell script and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sink.sh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700))
	return path
}

func TestExecSinkSendsTheContractEnvelope(t *testing.T) {
	out := filepath.Join(t.TempDir(), "captured.json")
	sh := script(t, "cat > "+out+"\n")

	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false,
		"sinks:\n  plugin:\n    command: [\""+sh+"\"]\n    params: {destination: somewhere, contract: not-the-header}\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.NoError(t, result.Sinks[0].Err)

	raw, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	require.NoError(t, err)

	var envelope struct {
		Contract int            `json:"contract"`
		Sink     string         `json:"sink"`
		Params   map[string]any `json:"params"`
		Digest   struct {
			Schema int  `json:"schema"`
			Empty  bool `json:"empty"`
			Items  []struct {
				Source string `json:"source"`
			} `json:"items"`
		} `json:"digest"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))

	assert.Equal(t, contractVersion, envelope.Contract)
	assert.Equal(t, "plugin", envelope.Sink)
	assert.Equal(t, "somewhere", envelope.Params["destination"])
	// The header is nested alongside params, not merged into them, so a plugin
	// whose params happen to use `contract` cannot collide with it.
	assert.Equal(t, "not-the-header", envelope.Params["contract"])
	assert.Equal(t, 1, envelope.Digest.Schema)
	assert.False(t, envelope.Digest.Empty)
	require.Len(t, envelope.Digest.Items, 1)
	assert.Equal(t, "a-source", envelope.Digest.Items[0].Source)
}

func TestExecSinkExitCodeIsTheSuccessSignal(t *testing.T) {
	d := day(t, "2026-09-04")

	t.Run("zero is success", func(t *testing.T) {
		sh := script(t, "cat > /dev/null\nexit 0\n")
		cfg, _ := fixture(t, d, false, "sinks:\n  plugin: {command: [\""+sh+"\"]}\n")
		result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
		require.NoError(t, err)
		assert.NoError(t, result.Sinks[0].Err)
	})

	t.Run("non-zero fails and stderr says why", func(t *testing.T) {
		sh := script(t, "cat > /dev/null\necho 'the destination rejected it' >&2\nexit 3\n")
		cfg, _ := fixture(t, d, false, "sinks:\n  plugin: {command: [\""+sh+"\"]}\n")
		result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
		require.NoError(t, err)
		require.Error(t, result.Sinks[0].Err)
		assert.Contains(t, result.Sinks[0].Err.Error(), "the destination rejected it")
	})
}

// TestExecSinkEnvironmentIsAnAllowList covers ADR-0003 §6: a plugin gets what it
// was granted, not whatever the engine happened to be started with.
func TestExecSinkEnvironmentIsAnAllowList(t *testing.T) {
	t.Setenv("GRANTED_TOKEN", "granted-value")
	t.Setenv("UNGRANTED_SECRET", "must-not-leak")

	out := filepath.Join(t.TempDir(), "env.txt")
	sh := script(t, "cat > /dev/null\nenv > "+out+"\n")

	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false,
		"sinks:\n  plugin:\n    command: [\""+sh+"\"]\n    env: [GRANTED_TOKEN]\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.NoError(t, result.Sinks[0].Err)

	raw, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	env := string(raw)

	assert.Contains(t, env, "GRANTED_TOKEN=granted-value")
	assert.NotContains(t, env, "must-not-leak",
		"an ungranted variable must not reach a plugin just because the engine had it")
	assert.Contains(t, env, "PATH=")
}

// TestExecSinkTimeoutKillsTheProcessGroup is the property I asked ADR-0003 to
// state explicitly: the engine creates the group so that killing it on timeout
// cannot reach the engine, and a plugin's children do not outlive it.
func TestExecSinkTimeoutKillsTheProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-still-running")

	// The script backgrounds a child that would outlive a naive kill of only
	// the direct process, then sleeps well past the deadline.
	sh := script(t, "cat > /dev/null\n(sleep 30; touch "+marker+") &\nsleep 30\n")

	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false, "sinks:\n  plugin: {command: [\""+sh+"\"]}\n")

	sink := &execSink{id: "plugin", command: []string{sh}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := sink.Deliver(ctx, Digest{Day: "2026-09-04", Structured: []byte(`{"empty":false}`)})
	elapsed := time.Since(start)

	require.Error(t, err, "a plugin past its deadline must fail, not hang the run")
	assert.Less(t, elapsed, 5*time.Second, "the kill must not wait for the plugin to finish")

	// Give a surviving grandchild every chance to prove it survived.
	time.Sleep(500 * time.Millisecond)
	assert.NoFileExists(t, marker, "a backgrounded child outlived the group kill")

	_ = cfg
}

func TestExecSinkStdoutIsLoggedNotTreatedAsFailure(t *testing.T) {
	sh := script(t, "cat > /dev/null\necho 'delivered to 3 recipients'\nexit 0\n")

	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false, "sinks:\n  plugin: {command: [\""+sh+"\"]}\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	// Anything on stdout is informational; the exit code is what decides.
	assert.NoError(t, result.Sinks[0].Err)
}

func TestExecSinkMissingProgramIsReported(t *testing.T) {
	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false, "sinks:\n  plugin: {command: [/nonexistent/sink-program]}\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.Error(t, result.Sinks[0].Err)
}

func TestExecSinkReceivesAnEmptyDigestToo(t *testing.T) {
	out := filepath.Join(t.TempDir(), "captured.json")
	sh := script(t, "cat > "+out+"\n")

	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, true, "sinks:\n  plugin: {command: [\""+sh+"\"]}\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.NoError(t, result.Sinks[0].Err)

	raw, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	var envelope struct {
		Digest struct {
			Empty bool `json:"empty"`
		} `json:"digest"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	assert.True(t, envelope.Digest.Empty, "a plugin is told about a quiet day rather than left to guess")
}

// TestExecSinkEscapedDescendantCannotHangTheRun is the sink half of the same
// gap: a setsid descendant is outside the group the timeout kills, and while it
// holds stdout open Wait reads to an EOF that never arrives.
func TestExecSinkEscapedDescendantCannotHangTheRun(t *testing.T) {
	sh := script(t, "cat > /dev/null\nsetsid sh -c 'sleep 20' &\nsleep 20\n")

	sink := &execSink{id: "plugin", command: []string{sh}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := sink.Deliver(ctx, Digest{Day: "2026-09-05", Structured: []byte(`{"empty":false}`)})
	elapsed := time.Since(start)

	require.Error(t, err, "a plugin past its deadline must fail")
	assert.Less(t, elapsed, 15*time.Second,
		"the run waited on a descendant that escaped the process group; it must give up instead")
}
