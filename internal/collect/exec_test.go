package collect

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/store"
)

// plugin writes an executable shell script and returns its path.
func plugin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.sh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700))
	return path
}

// execSource builds an exec source around a plugin path, with optional extra
// YAML for the entry (params, env). It goes through the real loader so a test
// runs against a source shaped exactly as production would accept it.
func execSource(t *testing.T, path, extra string) config.Source {
	t.Helper()

	entry := "  a-source:\n    collector: exec\n    cadence: daily\n    command: [\"" + path + "\"]\n"
	for _, line := range strings.Split(strings.TrimRight(extra, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			entry += "    " + line + "\n"
		}
	}
	return testConfig(t, t.TempDir(), entry).Sources["a-source"]
}

func execCollectorForTest() *execCollector { return &execCollector{log: quietLogger()} }

func TestExecCollectorSendsTheContractEnvelope(t *testing.T) {
	out := filepath.Join(t.TempDir(), "envelope.json")
	sh := plugin(t, "cat > "+out+"\n")

	src := execSource(t, sh, "params: {list: hot, contract: not-the-header}\n")
	_, err := execCollectorForTest().Collect(context.Background(), "bgg-hotness", src)
	require.NoError(t, err)

	raw, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	require.NoError(t, err)

	var got struct {
		Contract int            `json:"contract"`
		Source   string         `json:"source"`
		Params   map[string]any `json:"params"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))

	assert.Equal(t, contractVersion, got.Contract)
	assert.Equal(t, "bgg-hotness", got.Source, "the source id the engine knows must reach the plugin")
	assert.Equal(t, "hot", got.Params["list"])
	// The header is nested alongside params, not merged into them, so a plugin
	// whose params happen to use `contract` cannot collide with it.
	assert.Equal(t, "not-the-header", got.Params["contract"])
}

func TestExecCollectorParsesNDJSONItems(t *testing.T) {
	sh := plugin(t, `cat > /dev/null
echo '{"url":"https://example.com/a","title":"First","content":"body a"}'
echo '{"url":"https://example.com/b","content":"body b"}'
`)

	items, err := execCollectorForTest().Collect(context.Background(), "a-source", execSource(t, sh, ""))
	require.NoError(t, err)
	require.Len(t, items, 2)

	assert.Equal(t, "https://example.com/a", items[0].URL)
	assert.Equal(t, "First", items[0].Title)
	assert.Equal(t, "body a", items[0].Content)
	assert.Equal(t, "body b", items[1].Content)
	assert.Empty(t, items[1].Title, "title is optional provenance, not required")
}

// TestExecCollectorNeverTakesFetchedAtFromThePlugin is ADR-0003 §2's rule that
// stops a plugin writing into an immutable past day: the engine stamps
// fetched_at with its own clock, and a plugin time is provenance only.
func TestExecCollectorNeverTakesFetchedAtFromThePlugin(t *testing.T) {
	sh := plugin(t, `cat > /dev/null
echo '{"url":"https://example.com/a","content":"body","source_time":"2020-01-02T03:04:05Z","fetched_at":"2020-01-02T03:04:05Z"}'
`)

	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: exec, cadence: daily, command: [\""+sh+"\"]}\n")
	now := at(t, "2026-09-05T06:30:00Z")

	result := NewRunner(cfg,
		WithClock(func() time.Time { return now }),
		WithLogger(quietLogger()),
	).Run(context.Background())

	require.Len(t, result.Sources, 1)
	require.NoError(t, result.Sources[0].Err)
	require.Equal(t, 1, result.Sources[0].Written, "vacuity guard: the plugin's item must have been written")

	s := store.New(root)
	// The day folder is the ENGINE's day, not the plugin's.
	assert.DirExists(t, s.ItemsDir(now))
	assert.NoDirExists(t, s.ItemsDir(at(t, "2020-01-02T03:04:05Z")),
		"a plugin-supplied time must never choose the day key")

	// source_time survives as provenance in the item's front matter.
	entries, err := os.ReadDir(s.ItemsDir(now))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	body, err := os.ReadFile(filepath.Join(s.ItemsDir(now), entries[0].Name())) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(body), "2020-01-02", "the plugin's time is kept as source_time provenance")
	assert.Contains(t, string(body), "2026-09-05", "and the engine's own stamp is what fetched_at carries")
}

// TestExecCollectorSkipsMalformedLines is ADR-0003 §2: one bad item must not
// cost the batch.
func TestExecCollectorSkipsMalformedLines(t *testing.T) {
	sh := plugin(t, `cat > /dev/null
echo '{"url":"https://example.com/a","content":"good one"}'
echo 'this is not json at all'
echo '{"url":"https://example.com/b","content":'
echo ''
echo '{"url":"https://example.com/c","content":"good two"}'
`)

	items, err := execCollectorForTest().Collect(context.Background(), "a-source", execSource(t, sh, ""))
	require.NoError(t, err, "malformed lines are skipped, not fatal")
	require.Len(t, items, 2, "both good items survive the bad ones between them")
	assert.Equal(t, "good one", items[0].Content)
	assert.Equal(t, "good two", items[1].Content)
}

func TestExecCollectorSkipsItemsWithNoContent(t *testing.T) {
	sh := plugin(t, `cat > /dev/null
echo '{"url":"https://example.com/a","title":"no body"}'
echo '{"url":"https://example.com/b","content":"has a body"}'
`)

	items, err := execCollectorForTest().Collect(context.Background(), "a-source", execSource(t, sh, ""))
	require.NoError(t, err)
	require.Len(t, items, 1, "content is the one required field")
	assert.Equal(t, "has a body", items[0].Content)
}

// TestExecCollectorKeepsItemsEmittedBeforeAFailure is ADR-0003 §4: there is no
// partial-success protocol, but items already parsed were valid when parsed.
func TestExecCollectorKeepsItemsEmittedBeforeAFailure(t *testing.T) {
	sh := plugin(t, `cat > /dev/null
echo '{"url":"https://example.com/a","content":"emitted before the failure"}'
echo 'the upstream API rejected the second page' >&2
exit 4
`)

	items, err := execCollectorForTest().Collect(context.Background(), "a-source", execSource(t, sh, ""))

	require.Error(t, err, "a non-zero exit is a failed run")
	assert.Contains(t, err.Error(), "the upstream API rejected the second page", "stderr says why")
	require.Len(t, items, 1, "what the plugin did emit is kept")
	assert.Equal(t, "emitted before the failure", items[0].Content)
}

// TestExecCollectorEnvironmentIsAnAllowList covers ADR-0003 §6.
func TestExecCollectorEnvironmentIsAnAllowList(t *testing.T) {
	t.Setenv("GRANTED_TOKEN", "granted-value")
	t.Setenv("UNGRANTED_SECRET", "must-not-leak")

	out := filepath.Join(t.TempDir(), "env.txt")
	sh := plugin(t, "cat > /dev/null\nenv > "+out+"\n")

	src := execSource(t, sh, "env: [GRANTED_TOKEN]\n")
	_, err := execCollectorForTest().Collect(context.Background(), "a-source", src)
	require.NoError(t, err)

	raw, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	env := string(raw)

	assert.Contains(t, env, "GRANTED_TOKEN=granted-value")
	assert.NotContains(t, env, "must-not-leak",
		"an ungranted variable must not reach a plugin just because the engine had it")
	assert.Contains(t, env, "PATH=")
}

// TestExecCollectorTimeoutKillsTheProcessGroup is ADR-0003 §5: the engine makes
// the group so the kill cannot reach the engine, and a plugin's children do not
// outlive it holding the day's run hostage.
func TestExecCollectorTimeoutKillsTheProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-still-running")
	sh := plugin(t, "cat > /dev/null\n(sleep 30; touch "+marker+") &\nsleep 30\n")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := execCollectorForTest().Collect(ctx, "a-source", execSource(t, sh, ""))
	elapsed := time.Since(start)

	require.Error(t, err, "a plugin past its deadline must fail, not hang the pass")
	assert.Less(t, elapsed, 5*time.Second, "the kill must not wait for the plugin to finish")

	// Give a surviving grandchild every chance to prove it survived.
	time.Sleep(500 * time.Millisecond)
	assert.NoFileExists(t, marker, "a backgrounded child outlived the group kill")
}

func TestExecCollectorMissingProgramIsReported(t *testing.T) {
	src := testConfig(t, t.TempDir(),
		"  a-source: {collector: exec, cadence: daily, command: [/nonexistent/collector-plugin]}\n").Sources["a-source"]

	_, err := execCollectorForTest().Collect(context.Background(), "a-source", src)
	require.Error(t, err)
}

// TestExecCollectorItemIsSizeCapped is the ADR-0003 §2 price of the engine
// owning the disk: the whole stream passes through the engine's parser, so an
// oversize item is truncated with a marker rather than streamed.
func TestExecCollectorItemIsSizeCapped(t *testing.T) {
	oversize := maxItemBytes + 4096
	sh := plugin(t, "cat > /dev/null\n"+
		`python3 -c "import json;print(json.dumps({'url':'https://example.com/big','content':'x'*`+
		strconv.Itoa(oversize)+`}))"`+"\n")

	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: exec, cadence: daily, command: [\""+sh+"\"]}\n")
	now := at(t, "2026-09-05T06:30:00Z")

	result := NewRunner(cfg,
		WithClock(func() time.Time { return now }),
		WithLogger(quietLogger()),
	).Run(context.Background())

	require.Len(t, result.Sources, 1)
	require.NoError(t, result.Sources[0].Err)
	require.Equal(t, 1, result.Sources[0].Written)

	s := store.New(root)
	entries, err := os.ReadDir(s.ItemsDir(now))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	body, err := os.ReadFile(filepath.Join(s.ItemsDir(now), entries[0].Name())) //nolint:gosec // test-controlled path
	require.NoError(t, err)

	// Vacuity guard: the fixture really did produce an item past the cap.
	require.Greater(t, oversize, maxItemBytes, "the fixture must exceed the cap it is testing")
	assert.Contains(t, string(body), truncationMarker, "an oversize item is truncated with a marker")
	assert.Less(t, len(body), oversize, "and the oversize content did not reach disk whole")
}

// TestExecCollectorOversizeLineDoesNotCostTheBatch pins the reader's behaviour
// on a line too long to parse. ADR-0003 §2 requires that one bad item not cost
// the batch, and a bufio.Scanner would break that: it stops the whole stream at
// the first over-long token, silently dropping every item after it.
func TestExecCollectorOversizeLineDoesNotCostTheBatch(t *testing.T) {
	oversize := maxLineBytes + 1024
	sh := plugin(t, `cat > /dev/null
echo '{"url":"https://example.com/a","content":"before the giant"}'
python3 -c "print('{\"url\":\"https://example.com/huge\",\"content\":\"' + 'x'*`+strconv.Itoa(oversize)+` + '\"}')"
echo '{"url":"https://example.com/b","content":"after the giant"}'
`)

	items, err := execCollectorForTest().Collect(context.Background(), "a-source", execSource(t, sh, ""))
	require.NoError(t, err)

	// Vacuity guard: the fixture must really exceed the line limit, or this
	// test would pass against a reader that never had to skip anything.
	require.Greater(t, oversize, maxLineBytes, "the fixture must exceed the line limit it is testing")

	require.Len(t, items, 2, "the items either side of an unparsable line must both survive")
	assert.Equal(t, "before the giant", items[0].Content)
	assert.Equal(t, "after the giant", items[1].Content,
		"reading must continue past an oversize line, not stop at it")
}

// TestExecCollectorEscapedDescendantCannotHangThePass covers the gap the
// process-group kill alone leaves. A descendant that calls setsid is in its own
// session, so a negative-pid SIGKILL never reaches it — and while it holds the
// stdout pipe open, Wait keeps reading to EOF that never comes. The group kill
// succeeds and the pass hangs anyway, which is exactly the invariant the kill
// exists to protect: a hung plugin must not hold the day's run hostage.
//
// The in-group child of the timeout test cannot catch this, because it dies
// with the group and releases the pipe.
func TestExecCollectorEscapedDescendantCannotHangThePass(t *testing.T) {
	// The descendant escapes the group AND inherits stdout, so it holds the
	// pipe open long past the deadline.
	sh := plugin(t, "cat > /dev/null\nsetsid sh -c 'sleep 20' &\nsleep 20\n")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := execCollectorForTest().Collect(ctx, "a-source", execSource(t, sh, ""))
	elapsed := time.Since(start)

	require.Error(t, err, "a plugin past its deadline must fail")
	assert.Less(t, elapsed, 15*time.Second,
		"the pass waited on a descendant that escaped the process group; it must give up instead")
}
