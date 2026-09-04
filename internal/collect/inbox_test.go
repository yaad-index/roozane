package collect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/roozane/internal/store"
)

// inboxRunner builds a Runner with no sources, so a test exercises the drain
// alone, and returns it with the data root.
func inboxRunner(t *testing.T, now time.Time) (*Runner, string) {
	t.Helper()

	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: feed, cadence: daily}\n")

	r := NewRunner(cfg,
		WithClock(func() time.Time { return now }),
		WithCollector("feed", &stubCollector{}),
		WithLogger(quietLogger()),
	)
	require.NoError(t, os.MkdirAll(store.New(root).InboxDir(), 0o755))
	return r, root
}

func drop(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(store.New(root).InboxDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestDrainInbox(t *testing.T) {
	now := at(t, "2026-09-04T06:30:00Z")
	r, root := inboxRunner(t, now)

	original := drop(t, root, "newsletter.md", "the newsletter body")

	result := r.Run(context.Background())
	require.NoError(t, result.InboxErr)
	assert.Equal(t, 1, result.InboxDrained)

	// The original is gone only because the normalised item is in place: the
	// inbox is a queue, not an archive.
	assert.NoFileExists(t, original)

	items, err := os.ReadDir(store.New(root).ItemsDir(now))
	require.NoError(t, err)
	require.Len(t, items, 1)

	raw, err := os.ReadFile(filepath.Join(store.New(root).ItemsDir(now), items[0].Name())) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	body := string(raw)

	assert.Contains(t, body, "source: inbox")
	assert.Contains(t, body, "collector: inbox")
	assert.Contains(t, body, "original_filename: newsletter.md")
	assert.Contains(t, body, "fetched_at: \"2026-09-04T06:30:00Z\"")
	assert.Contains(t, body, "the newsletter body")
}

func TestDrainInboxSkipsPartialDrops(t *testing.T) {
	now := at(t, "2026-09-04T06:30:00Z")
	r, root := inboxRunner(t, now)

	// ADR-0002 pairs an atomic-rename requirement on producers with this skip
	// rule. Both names are what an in-progress write looks like, so reading
	// either would be reading a half-written file.
	partialTmp := drop(t, root, "in-progress.tmp", "half a newsletter")
	partialDot := drop(t, root, ".in-progress", "half a newsletter")
	finished := drop(t, root, "finished.md", "a whole newsletter")

	result := r.Run(context.Background())
	require.NoError(t, result.InboxErr)
	assert.Equal(t, 1, result.InboxDrained)

	// The skipped drops are still there, untouched, for a later run.
	assert.FileExists(t, partialTmp)
	assert.FileExists(t, partialDot)
	assert.NoFileExists(t, finished)

	items, err := os.ReadDir(store.New(root).ItemsDir(now))
	require.NoError(t, err)
	require.Len(t, items, 1)

	raw, err := os.ReadFile(filepath.Join(store.New(root).ItemsDir(now), items[0].Name())) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(raw), "a whole newsletter")
}

func TestDrainInboxLeavesEmptyFilesInPlace(t *testing.T) {
	now := at(t, "2026-09-04T06:30:00Z")
	r, root := inboxRunner(t, now)

	empty := drop(t, root, "empty.md", "")

	result := r.Run(context.Background())
	require.NoError(t, result.InboxErr)
	assert.Zero(t, result.InboxDrained)

	// Deleting a file the reader deliberately placed there is not this code's
	// call to make, so it stays and the run says nothing was drained.
	assert.FileExists(t, empty)
	assert.NoDirExists(t, store.New(root).ItemsDir(now))
}

func TestDrainInboxIsFineWithNoInboxDirectory(t *testing.T) {
	now := at(t, "2026-09-04T06:30:00Z")
	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: feed, cadence: daily}\n")

	// A fresh install has no inbox yet. That is the normal state, not a fault.
	result := NewRunner(cfg,
		WithClock(func() time.Time { return now }),
		WithCollector("feed", &stubCollector{}),
		WithLogger(quietLogger()),
	).Run(context.Background())

	assert.NoError(t, result.InboxErr)
	assert.Zero(t, result.InboxDrained)
	assert.False(t, result.Failed())
}

func TestDrainInboxTruncatesOversizeDrops(t *testing.T) {
	now := at(t, "2026-09-04T06:30:00Z")
	r, root := inboxRunner(t, now)

	drop(t, root, "huge.md", strings.Repeat("a", maxItemBytes+100))

	result := r.Run(context.Background())
	require.NoError(t, result.InboxErr)
	assert.Equal(t, 1, result.InboxDrained)

	items, err := os.ReadDir(store.New(root).ItemsDir(now))
	require.NoError(t, err)
	require.Len(t, items, 1)

	raw, err := os.ReadFile(filepath.Join(store.New(root).ItemsDir(now), items[0].Name())) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(raw), "truncated at the size cap")
}

func TestDrainInboxSkipsDirectories(t *testing.T) {
	now := at(t, "2026-09-04T06:30:00Z")
	r, root := inboxRunner(t, now)

	require.NoError(t, os.MkdirAll(filepath.Join(store.New(root).InboxDir(), "a-folder"), 0o755))
	drop(t, root, "a-file.md", "body")

	result := r.Run(context.Background())
	require.NoError(t, result.InboxErr)
	assert.Equal(t, 1, result.InboxDrained)
	assert.DirExists(t, filepath.Join(store.New(root).InboxDir(), "a-folder"))
}

func TestDrainableRule(t *testing.T) {
	for name, want := range map[string]bool{
		"newsletter.md":    true,
		"no-extension":     true,
		"weird.tmp.md":     true,
		"in-progress.tmp":  false,
		".hidden":          false,
		".in-progress.tmp": false,
		"trailing.tmp.":    true,
		"UPPER.TMP":        true, // the rule is exact; a producer follows it literally
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, drainable(name))
		})
	}
}
