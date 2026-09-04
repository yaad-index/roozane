package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}

func TestItemKeyPrefersTheURL(t *testing.T) {
	// Keying on the URL is what makes a re-run rewrite the same file even when
	// the page's text has changed since the first fetch.
	first := ItemKey("https://example.com/a", "text as it was this morning")
	second := ItemKey("https://example.com/a", "text as it is now")
	assert.Equal(t, first, second)

	assert.NotEqual(t, first, ItemKey("https://example.com/b", "text as it was this morning"))

	// With no URL the content is the identity, so the same drop twice is one
	// file and different text is two.
	assert.Equal(t, ItemKey("", "same body"), ItemKey("", "same body"))
	assert.NotEqual(t, ItemKey("", "one body"), ItemKey("", "another body"))

	assert.Len(t, ItemKey("https://example.com/a", ""), keyLength)
}

func TestFilename(t *testing.T) {
	name := Filename("hn-frontpage", "https://example.com/a", "")

	assert.True(t, strings.HasPrefix(name, "hn-frontpage--"), name)
	assert.True(t, strings.HasSuffix(name, ".md"), name)
}

func TestDayIsUTC(t *testing.T) {
	// 01:30 on the 5th in Berlin is still the 4th in UTC. The day key has to
	// follow UTC or collection, aggregation and retention stop agreeing.
	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)

	local := time.Date(2026, 9, 5, 1, 30, 0, 0, berlin)
	assert.Equal(t, "2026-09-04", Day(local))
}

func TestWriteItem(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	at := mustTime(t, "2026-09-04T06:30:00Z")

	path, err := s.WriteItem(Item{
		Source:     "hn-frontpage",
		URL:        "https://example.com/a",
		Title:      "A headline",
		FetchedAt:  at,
		SourceTime: mustTime(t, "2026-09-03T22:00:00Z"),
		Collector:  "feed",
		Content:    "the raw text",
	})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(root, "days", "2026-09-04", "items"), filepath.Dir(path))

	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	body := string(raw)

	assert.True(t, strings.HasPrefix(body, "---\n"), body)
	assert.Contains(t, body, "source: hn-frontpage\n")
	assert.Contains(t, body, "url: https://example.com/a\n")
	assert.Contains(t, body, "title: A headline\n")
	assert.Contains(t, body, "fetched_at: \"2026-09-04T06:30:00Z\"\n")
	assert.Contains(t, body, "source_time: \"2026-09-03T22:00:00Z\"\n")
	assert.Contains(t, body, "collector: feed\n")
	assert.True(t, strings.HasSuffix(body, "---\nthe raw text\n"), body)

	// Optional fields are omitted rather than written empty, so a hand-read
	// file carries no noise.
	assert.NotContains(t, body, "original_filename:")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestWriteItemLeavesNoTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	at := mustTime(t, "2026-09-04T06:30:00Z")

	_, err := s.WriteItem(Item{Source: "a-source", FetchedAt: at, Collector: "http", Content: "body"})
	require.NoError(t, err)

	entries, err := os.ReadDir(s.ItemsDir(at))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	// A leftover temp file would be read by the aggregator as a mystery item.
	assert.False(t, strings.HasPrefix(entries[0].Name(), ".tmp-"), entries[0].Name())
}

func TestWriteItemIsIdempotentWithinADay(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	at := mustTime(t, "2026-09-04T06:30:00Z")

	item := Item{Source: "a-source", URL: "https://example.com/a", FetchedAt: at, Collector: "http", Content: "first text"}
	first, err := s.WriteItem(item)
	require.NoError(t, err)

	item.Content = "second text"
	second, err := s.WriteItem(item)
	require.NoError(t, err)

	// Same identity, same filename: a re-run rewrites rather than appending.
	assert.Equal(t, first, second)

	entries, err := os.ReadDir(s.ItemsDir(at))
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	raw, err := os.ReadFile(second) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(raw), "second text")
}

func TestWriteItemSeparatesDays(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	item := Item{Source: "a-source", URL: "https://example.com/a", Collector: "http", Content: "body"}

	item.FetchedAt = mustTime(t, "2026-09-04T06:30:00Z")
	first, err := s.WriteItem(item)
	require.NoError(t, err)

	item.FetchedAt = mustTime(t, "2026-09-05T06:30:00Z")
	second, err := s.WriteItem(item)
	require.NoError(t, err)

	// Same identity, different day: each day keeps its own snapshot, which is
	// what makes "what did this say on the 4th" answerable.
	assert.NotEqual(t, first, second)
	assert.Equal(t, filepath.Base(first), filepath.Base(second))
}

func TestWriteItemRejectsUnstampedItems(t *testing.T) {
	s := New(t.TempDir())
	at := mustTime(t, "2026-09-04T06:30:00Z")

	for name, tc := range map[string]struct {
		item Item
		want string
	}{
		"no source":     {item: Item{FetchedAt: at, Collector: "http", Content: "b"}, want: "no source"},
		"no collector":  {item: Item{Source: "a", FetchedAt: at, Content: "b"}, want: "no collector"},
		"no fetched_at": {item: Item{Source: "a", Collector: "http", Content: "b"}, want: "no fetched_at"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.WriteItem(tc.item)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestSourceLastCollected(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-10T12:00:00Z")

	_, found, err := s.SourceLastCollected("a-source", now, 30)
	require.NoError(t, err)
	assert.False(t, found, "a source that never ran has no last-collected day")

	_, err = s.WriteItem(Item{
		Source:    "a-source",
		FetchedAt: now.AddDate(0, 0, -3),
		Collector: "http",
		Content:   "body",
	})
	require.NoError(t, err)

	last, found, err := s.SourceLastCollected("a-source", now, 30)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "2026-09-07", Day(last))

	// Outside the window is the same answer as never, because the caller only
	// looks back as far as the cadence it is checking.
	_, found, err = s.SourceLastCollected("a-source", now, 2)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestSourceLastCollectedDoesNotMatchAPrefixOfAnotherID(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-10T12:00:00Z")

	_, err := s.WriteItem(Item{Source: "hn-front", FetchedAt: now, Collector: "http", Content: "body"})
	require.NoError(t, err)

	// "hn" is a prefix of "hn-front", but the separator has to follow the id
	// immediately, so it must not be credited with another source's items.
	_, found, err := s.SourceLastCollected("hn", now, 30)
	require.NoError(t, err)
	assert.False(t, found)

	_, found, err = s.SourceLastCollected("hn-front", now, 30)
	require.NoError(t, err)
	assert.True(t, found)
}

// TestWriteItemNeverExposesATornFile is the test for the invariant ADR-0002
// states directly: "a re-run must never expose a torn file to a concurrently
// reading aggregator". It asserts the observable property rather than the
// mechanism — a reader racing a rewrite must always see one whole document,
// never a half-written one.
//
// A plain os.WriteFile to the destination fails this: the truncate-then-write
// leaves a window in which the file on disk is short.
func TestWriteItemNeverExposesATornFile(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	at := mustTime(t, "2026-09-04T06:30:00Z")

	// Large enough that a non-atomic write cannot complete between a reader's
	// open and its read.
	bodyA := strings.Repeat("A", 512*1024)
	bodyB := strings.Repeat("B", 512*1024)

	item := Item{Source: "a-source", URL: "https://example.com/a", FetchedAt: at, Collector: "http", Content: bodyA}
	path, err := s.WriteItem(item)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			item.Content = bodyA
			if i%2 == 1 {
				item.Content = bodyB
			}
			if _, err := s.WriteItem(item); err != nil {
				return
			}
		}
	}()

	var reads int
	for {
		select {
		case <-done:
			// The writer must have been racing a real reader for this to mean
			// anything, so prove the loop below actually ran.
			assert.Greater(t, reads, 10, "the reader has to have raced the writer for this test to say anything")
			return
		default:
		}

		raw, err := os.ReadFile(path) //nolint:gosec // test-controlled path
		if os.IsNotExist(err) {
			// Never expected with rename-into-place; a direct write would not
			// produce this either, so it is not the failure being hunted.
			continue
		}
		require.NoError(t, err)
		reads++

		body := string(raw)
		require.True(t, strings.HasPrefix(body, "---\n"), "read a file with no front matter: torn write")
		// Every complete document ends with the whole body and a newline. A
		// torn read lands mid-content and fails here.
		require.True(t,
			strings.HasSuffix(body, bodyA+"\n") || strings.HasSuffix(body, bodyB+"\n"),
			"read a file whose content is neither complete body: torn write (len %d)", len(raw))
	}
}
