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

// TestWriteAtomicNeverExposesATornFile covers the other writer. WriteItem and
// WriteAtomic are separate entry points, so a change that made WriteAtomic
// write in place would leave the item test green while digests — which sinks
// read concurrently — became torn-readable.
func TestWriteAtomicNeverExposesATornFile(t *testing.T) {
	s := New(t.TempDir())
	path := filepath.Join(s.EditionDir("default"), "2026-09-04.json")

	bodyA := "A" + strings.Repeat("a", 512*1024)
	bodyB := "B" + strings.Repeat("b", 512*1024)

	require.NoError(t, s.WriteAtomic(path, []byte(bodyA)))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			body := bodyA
			if i%2 == 1 {
				body = bodyB
			}
			if err := s.WriteAtomic(path, []byte(body)); err != nil {
				return
			}
		}
	}()

	var reads int
	for {
		select {
		case <-done:
			assert.Greater(t, reads, 10, "the reader has to have raced the writer for this to say anything")
			return
		default:
		}

		raw, err := os.ReadFile(path) //nolint:gosec // test-controlled path
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		reads++

		body := string(raw)
		require.True(t, body == bodyA || body == bodyB,
			"read a partial digest: torn write (len %d, starts %q)", len(body), body[:min(1, len(body))])
	}
}

func TestMarkRanWritesAnEmptyMarkerNamedForTheSource(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-10T12:00:00Z")

	require.NoError(t, s.MarkRan("a-source", now))

	path := filepath.Join(s.RanDir(now), "a-source")
	info, err := os.Stat(path)
	require.NoError(t, err, "the marker must be named for the source, with nothing appended")
	assert.Zero(t, info.Size(), "the name and the day folder are the whole datum; the file carries nothing")

	// A sibling of items/, not a member of it — readers of items/ must not have
	// to learn to skip anything.
	assert.Equal(t, filepath.Join(s.DayDir(now), "ran"), s.RanDir(now))
	assert.NotEqual(t, s.ItemsDir(now), s.RanDir(now))
}

func TestMarkRanIsIdempotentWithinADay(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-10T12:00:00Z")

	require.NoError(t, s.MarkRan("a-source", now))
	require.NoError(t, s.MarkRan("a-source", now.Add(3*time.Hour)),
		"a second empty run the same day must not fail")

	entries, err := os.ReadDir(s.RanDir(now))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the same day must hold one marker per source, not one per run")
	assert.Equal(t, "a-source", entries[0].Name())
}

func TestMarkRanRejectsAnUnstampedMarker(t *testing.T) {
	s := New(t.TempDir())

	require.Error(t, s.MarkRan("", mustTime(t, "2026-09-10T12:00:00Z")))
	require.Error(t, s.MarkRan("a-source", time.Time{}),
		"an unstamped marker would land in whatever day the zero time falls in")
}

// TestSourceLastCollectedCountsAnEmptyRun is the property ADR-0004 exists for:
// a source that ran and found nothing must read as having run, not as never
// collected.
func TestSourceLastCollectedCountsAnEmptyRun(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-10T12:00:00Z")

	_, found, err := s.SourceLastCollected("a-source", now, 30)
	require.NoError(t, err)
	require.False(t, found, "vacuity guard: the source must start out never-collected")

	require.NoError(t, s.MarkRan("a-source", now.AddDate(0, 0, -3)))

	last, found, err := s.SourceLastCollected("a-source", now, 30)
	require.NoError(t, err)
	require.True(t, found, "a marker must count as a run even with no items beside it")
	assert.Equal(t, "2026-09-07", Day(last))

	// The marker obeys the same window as an item: past the lookback it is the
	// same answer as never.
	_, found, err = s.SourceLastCollected("a-source", now, 2)
	require.NoError(t, err)
	assert.False(t, found)
}

// TestSourceLastCollectedTakesTheLaterOfItemAndMarker runs both orderings.
// One direction alone cannot expose a mistake here: an implementation that
// only ever consulted items would still pass the marker-older case.
func TestSourceLastCollectedTakesTheLaterOfItemAndMarker(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")

	t.Run("marker is the later one", func(t *testing.T) {
		s := New(t.TempDir())
		_, err := s.WriteItem(Item{
			Source: "a-source", FetchedAt: now.AddDate(0, 0, -8),
			Collector: "http", Content: "body",
		})
		require.NoError(t, err)
		require.NoError(t, s.MarkRan("a-source", now.AddDate(0, 0, -2)))

		last, found, err := s.SourceLastCollected("a-source", now, 30)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "2026-09-08", Day(last),
			"a source that found items once and nothing later last ran on the later day")
	})

	t.Run("item is the later one", func(t *testing.T) {
		s := New(t.TempDir())
		require.NoError(t, s.MarkRan("a-source", now.AddDate(0, 0, -8)))
		_, err := s.WriteItem(Item{
			Source: "a-source", FetchedAt: now.AddDate(0, 0, -2),
			Collector: "http", Content: "body",
		})
		require.NoError(t, err)

		last, found, err := s.SourceLastCollected("a-source", now, 30)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "2026-09-08", Day(last),
			"an older marker must not pull the last-run day backwards")
	})
}

// TestSourceLastCollectedIgnoresATemporaryMarkerFile guards the exact-name
// match. writeFileAtomic stages every write as `.tmp-*` in the destination
// directory, so a crash mid-write can leave one behind — and in ran/ the
// filename IS the source id, so a reader that accepted any entry would credit
// a source with a run it never had.
func TestSourceLastCollectedIgnoresATemporaryMarkerFile(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-10T12:00:00Z")

	require.NoError(t, os.MkdirAll(s.RanDir(now), 0o755))
	stranded := filepath.Join(s.RanDir(now), ".tmp-834217")
	require.NoError(t, os.WriteFile(stranded, nil, 0o644))

	_, found, err := s.SourceLastCollected("a-source", now, 30)
	require.NoError(t, err)
	assert.False(t, found, "a stranded temporary file is not a run marker")

	// Vacuity guard: the directory really does hold the file under test.
	entries, err := os.ReadDir(s.RanDir(now))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestSourceLastCollectedMarkerDoesNotMatchAPrefixOfAnotherID(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-10T12:00:00Z")

	require.NoError(t, s.MarkRan("hn-front", now))

	_, found, err := s.SourceLastCollected("hn", now, 30)
	require.NoError(t, err)
	assert.False(t, found, "one source's marker must not be credited to another")

	_, found, err = s.SourceLastCollected("hn-front", now, 30)
	require.NoError(t, err)
	assert.True(t, found)
}

// seedDay creates a day folder with one item in it, so a prune has something
// real to remove.
func seedDay(t *testing.T, s *Store, day time.Time) {
	t.Helper()
	_, err := s.WriteItem(Item{
		Source: "a-source", FetchedAt: day, Collector: "http", Content: "body",
	})
	require.NoError(t, err)
}

// TestPruneKeepsExactlyTheNewestNDays pins the cutoff the config validation
// depends on: a day survives while its age is strictly less than the window.
func TestPruneKeepsExactlyTheNewestNDays(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-30T12:00:00Z")

	for age := 0; age <= 9; age++ {
		seedDay(t, s, now.AddDate(0, 0, -age))
	}

	result, err := s.Prune(7, 0, now)
	require.NoError(t, err)
	assert.Equal(t, 3, result.Days, "ages 7, 8 and 9 are past a 7-day window")

	// Ages 0..6 survive; 7 and older do not. The boundary pair is the point:
	// age 6 present and age 7 absent is what makes the window mean "newest 7".
	for age := 0; age <= 6; age++ {
		assert.DirExists(t, s.DayDir(now.AddDate(0, 0, -age)),
			"age %d is inside a 7-day window and must survive", age)
	}
	for age := 7; age <= 9; age++ {
		assert.NoDirExists(t, s.DayDir(now.AddDate(0, 0, -age)),
			"age %d is past a 7-day window and must be pruned", age)
	}
}

func TestPruneWithAWindowOfOneKeepsTodayAlone(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-30T12:00:00Z")

	seedDay(t, s, now)
	seedDay(t, s, now.AddDate(0, 0, -1))

	result, err := s.Prune(1, 0, now)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Days)

	assert.DirExists(t, s.DayDir(now), "the day collectors are writing into must survive")
	assert.NoDirExists(t, s.DayDir(now.AddDate(0, 0, -1)))
}

// TestPruneWithAWindowBelowOneRemovesNothing — validation rejects such a window,
// so reaching here is a bug, and the safe reading of a bug is to delete nothing.
func TestPruneWithAWindowBelowOneRemovesNothing(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-30T12:00:00Z")

	seedDay(t, s, now.AddDate(0, 0, -400))

	for _, window := range []int{0, -1} {
		result, err := s.Prune(window, 0, now)
		require.NoError(t, err)
		assert.Zero(t, result.Days, "window %d must prune nothing", window)
		assert.DirExists(t, s.DayDir(now.AddDate(0, 0, -400)))
	}
}

// TestPruneLeavesUnrecognisedEntriesAlone — a retention setting is permission to
// delete this engine's own old days, not whatever else shares the directory.
func TestPruneLeavesUnrecognisedEntriesAlone(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-30T12:00:00Z")

	seedDay(t, s, now.AddDate(0, 0, -30))
	stray := filepath.Join(root, "days", "notes-from-the-operator")
	require.NoError(t, os.MkdirAll(stray, 0o755))

	result, err := s.Prune(7, 0, now)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Days, "only the real day folder counts as pruned")
	assert.DirExists(t, stray, "a name that is not a day key must not be touched")
}

func TestPruneDigestsUsesItsOwnWindow(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-30T12:00:00Z")

	require.NoError(t, os.MkdirAll(s.EditionDir("default"), 0o755))
	write := func(day time.Time) (string, string) {
		md, structured := s.DigestPaths(day, "default")
		require.NoError(t, os.WriteFile(md, []byte("# digest"), 0o644))
		require.NoError(t, os.WriteFile(structured, []byte("{}"), 0o644))
		return md, structured
	}
	keptMD, keptJSON := write(now.AddDate(0, 0, -2))
	goneMD, goneJSON := write(now.AddDate(0, 0, -3))

	result, err := s.Prune(90, 3, now)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Digests, "both files of one day are one day's digest")
	assert.Zero(t, result.Days, "the item window is separate and untouched here")

	assert.FileExists(t, keptMD)
	assert.FileExists(t, keptJSON)
	assert.NoFileExists(t, goneMD)
	assert.NoFileExists(t, goneJSON)
}

// TestPruneDigestsZeroKeepsForever is the documented default.
func TestPruneDigestsZeroKeepsForever(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-30T12:00:00Z")

	require.NoError(t, os.MkdirAll(s.EditionDir("default"), 0o755))
	md, _ := s.DigestPaths(now.AddDate(0, 0, -4000), "default")
	require.NoError(t, os.WriteFile(md, []byte("# ancient"), 0o644))

	result, err := s.Prune(90, 0, now)
	require.NoError(t, err)
	assert.Zero(t, result.Digests)
	assert.FileExists(t, md, "zero means keep forever, not keep nothing")
}

func TestPruneDigestsLeavesOtherFilesAlone(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-30T12:00:00Z")

	require.NoError(t, os.MkdirAll(s.EditionDir("default"), 0o755))
	readme := filepath.Join(s.EditionDir("default"), "README.md")
	require.NoError(t, os.WriteFile(readme, []byte("mine"), 0o644))
	notADay := filepath.Join(s.EditionDir("default"), "summary-2026.json")
	require.NoError(t, os.WriteFile(notADay, []byte("{}"), 0o644))

	// A stray file sitting directly under digests/ is not a digest under the
	// nested layout either, and removing whatever happens to be there is not
	// the pruner's business.
	strayAtRoot := filepath.Join(s.DigestsDir(), "2026-01-01.md")
	require.NoError(t, os.WriteFile(strayAtRoot, []byte("old layout"), 0o644))

	result, err := s.Prune(90, 1, now)
	require.NoError(t, err)
	assert.Zero(t, result.Digests)
	assert.FileExists(t, readme)
	assert.FileExists(t, notADay)
	assert.FileExists(t, strayAtRoot)
}

func TestPruneOnAnEmptyDataRootIsNotAnError(t *testing.T) {
	s := New(t.TempDir())
	result, err := s.Prune(7, 7, mustTime(t, "2026-09-30T12:00:00Z"))
	require.NoError(t, err, "a fresh install has neither tree yet")
	assert.Zero(t, result.Days)
	assert.Zero(t, result.Digests)
}

// --- digests nested under an edition (ADR-0005 §4) ---

func TestDigestPathsAreNestedUnderTheEdition(t *testing.T) {
	s := New("/srv/roozane")
	day := mustTime(t, "2026-09-04T00:00:00Z")

	md, structured := s.DigestPaths(day, "boardgames")
	assert.Equal(t, "/srv/roozane/digests/boardgames/2026-09-04.md", md)
	assert.Equal(t, "/srv/roozane/digests/boardgames/2026-09-04.json", structured)

	// The single-reader case takes the same shape rather than a flat path, so
	// the layout has one rule instead of a rule and an exception.
	md, _ = s.DigestPaths(day, "default")
	assert.Equal(t, "/srv/roozane/digests/default/2026-09-04.md", md)
}

// TestPruneDigestsDescendsIntoEveryEdition is the case that silently stopped
// working when digests moved: the pruner skipped directory entries outright, so
// a configured window became keep-forever with no error and no log line — a
// failure noticed by a full disk.
func TestPruneDigestsDescendsIntoEveryEdition(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := mustTime(t, "2026-09-30T12:00:00Z")

	write := func(edition string, day time.Time) (string, string) {
		require.NoError(t, os.MkdirAll(s.EditionDir(edition), 0o755))
		md, structured := s.DigestPaths(day, edition)
		require.NoError(t, os.WriteFile(md, []byte("# digest"), 0o644))
		require.NoError(t, os.WriteFile(structured, []byte("{}"), 0o644))
		return md, structured
	}

	personalKept, _ := write("personal", now.AddDate(0, 0, -2))
	personalGone, personalGoneJSON := write("personal", now.AddDate(0, 0, -3))
	boardKept, _ := write("boardgames", now.AddDate(0, 0, -1))
	boardGone, boardGoneJSON := write("boardgames", now.AddDate(0, 0, -9))

	result, err := s.Prune(90, 3, now)
	require.NoError(t, err)

	// Four files across two editions: the window has to be applied inside each
	// one, not to the top level where nothing but directories live.
	assert.Equal(t, 4, result.Digests)
	assert.FileExists(t, personalKept)
	assert.FileExists(t, boardKept)
	assert.NoFileExists(t, personalGone)
	assert.NoFileExists(t, personalGoneJSON)
	assert.NoFileExists(t, boardGone)
	assert.NoFileExists(t, boardGoneJSON)
}
