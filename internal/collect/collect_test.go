package collect

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// quietLogger keeps test output readable; the assertions are on behaviour, not
// on log lines.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}

// testConfig writes a config with the given sources block and loads it through
// the real loader, so every test runs against a config that would be accepted
// in production rather than a hand-built struct.
func testConfig(t *testing.T, dataRoot, sources string) *config.Config {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "roozane.yaml")
	body := "data_root: " + dataRoot + `
relevance_profile: profile.md
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small, digest: large}
sources:
` + sources

	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err)
	return cfg
}

// stubCollector returns fixed items, or an error, and records its calls.
type stubCollector struct {
	items []Collected
	err   error
	calls int
}

func (s *stubCollector) Collect(context.Context, config.Source) ([]Collected, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.items, nil
}

func TestRunWritesItemsStampedByTheEngine(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: feed, cadence: daily}\n")
	now := at(t, "2026-09-04T06:30:00Z")

	stub := &stubCollector{items: []Collected{
		{URL: "https://example.com/a", Title: "First", Content: "first body", SourceTime: at(t, "2001-01-01T00:00:00Z")},
		{URL: "https://example.com/b", Title: "Second", Content: "second body"},
	}}

	result := NewRunner(cfg,
		WithClock(func() time.Time { return now }),
		WithCollector("feed", stub),
		WithLogger(quietLogger()),
	).Run(context.Background())

	require.False(t, result.Failed())
	require.Len(t, result.Sources, 1)
	assert.Equal(t, 2, result.Sources[0].Written)

	items, err := os.ReadDir(store.New(root).ItemsDir(now))
	require.NoError(t, err)
	assert.Len(t, items, 2)

	raw, err := os.ReadFile(filepath.Join(store.New(root).ItemsDir(now), store.Filename("a-source", "https://example.com/a", ""))) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	body := string(raw)

	// The engine's clock chooses the day and stamps fetched_at; the item's own
	// claimed time is kept as provenance and must not have chosen anything.
	assert.Contains(t, body, "fetched_at: \"2026-09-04T06:30:00Z\"")
	assert.Contains(t, body, "source_time: \"2001-01-01T00:00:00Z\"")
	assert.Contains(t, body, "collector: feed")
	assert.Contains(t, body, "first body")
}

func TestRunSkipsItemsWithNoContent(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: feed, cadence: daily}\n")
	now := at(t, "2026-09-04T06:30:00Z")

	stub := &stubCollector{items: []Collected{
		{URL: "https://example.com/a", Content: ""},
		{URL: "https://example.com/b", Content: "real body"},
	}}

	result := NewRunner(cfg,
		WithClock(func() time.Time { return now }),
		WithCollector("feed", stub),
		WithLogger(quietLogger()),
	).Run(context.Background())

	// An empty item would land as a file that looks like a collection which
	// happened, so it is dropped rather than written.
	assert.Equal(t, 1, result.Sources[0].Written)
	items, err := os.ReadDir(store.New(root).ItemsDir(now))
	require.NoError(t, err)
	assert.Len(t, items, 1)
}

func TestRunOneFailingSourceDoesNotStopTheOthers(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root,
		"  a-source: {collector: feed, cadence: daily}\n"+
			"  b-source: {collector: http, cadence: daily}\n")
	now := at(t, "2026-09-04T06:30:00Z")

	result := NewRunner(cfg,
		WithClock(func() time.Time { return now }),
		WithCollector("feed", &stubCollector{err: errors.New("feed is down")}),
		WithCollector("http", &stubCollector{items: []Collected{{URL: "https://example.com/b", Content: "body"}}}),
		WithLogger(quietLogger()),
	).Run(context.Background())

	require.Len(t, result.Sources, 2)
	// Sources are reported in sorted id order, so this is deterministic.
	assert.Equal(t, "a-source", result.Sources[0].ID)
	require.Error(t, result.Sources[0].Err)
	assert.Equal(t, "b-source", result.Sources[1].ID)
	assert.NoError(t, result.Sources[1].Err)
	assert.Equal(t, 1, result.Sources[1].Written)

	// One broken feed must not cost the reader the rest of the digest, but the
	// pass still reports failure so a scheduler notices.
	assert.True(t, result.Failed())
}

func TestCadenceSkipsSourcesThatAreNotDue(t *testing.T) {
	for name, tc := range map[string]struct {
		cadence  string
		lastDays int // how many days ago the source last wrote
		wantDue  bool
	}{
		"daily, collected today":     {cadence: "daily", lastDays: 0, wantDue: false},
		"daily, collected yesterday": {cadence: "daily", lastDays: 1, wantDue: true},
		"weekly, six days ago":       {cadence: "weekly", lastDays: 6, wantDue: false},
		"weekly, seven days ago":     {cadence: "weekly", lastDays: 7, wantDue: true},
		"monthly, twenty-nine ago":   {cadence: "monthly", lastDays: 29, wantDue: false},
		"monthly, thirty days ago":   {cadence: "monthly", lastDays: 30, wantDue: true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			cfg := testConfig(t, root, "  a-source: {collector: feed, cadence: "+tc.cadence+"}\n")
			now := at(t, "2026-09-30T12:00:00Z")

			// Seed a previous collection at the stated age.
			_, err := store.New(root).WriteItem(store.Item{
				Source:    "a-source",
				URL:       "https://example.com/seed",
				FetchedAt: now.AddDate(0, 0, -tc.lastDays),
				Collector: "feed",
				Content:   "seed",
			})
			require.NoError(t, err)

			stub := &stubCollector{items: []Collected{{URL: "https://example.com/new", Content: "new body"}}}
			result := NewRunner(cfg,
				WithClock(func() time.Time { return now }),
				WithCollector("feed", stub),
				WithLogger(quietLogger()),
			).Run(context.Background())

			require.Len(t, result.Sources, 1)
			if tc.wantDue {
				assert.False(t, result.Sources[0].Skipped)
				assert.Equal(t, 1, stub.calls, "a due source must be fetched")
			} else {
				assert.True(t, result.Sources[0].Skipped)
				assert.Zero(t, stub.calls, "a source that is not due must not be fetched at all")
			}
		})
	}
}

func TestFirstRunIsAlwaysDue(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: feed, cadence: monthly}\n")
	now := at(t, "2026-09-04T06:30:00Z")

	stub := &stubCollector{items: []Collected{{URL: "https://example.com/a", Content: "body"}}}
	result := NewRunner(cfg,
		WithClock(func() time.Time { return now }),
		WithCollector("feed", stub),
		WithLogger(quietLogger()),
	).Run(context.Background())

	// Nothing on disk means nothing to wait for, whatever the cadence.
	assert.False(t, result.Sources[0].Skipped)
	assert.Equal(t, 1, stub.calls)
}

func TestCapContent(t *testing.T) {
	small := strings.Repeat("a", 10)
	out, truncated := capContent(small)
	assert.False(t, truncated)
	assert.Equal(t, small, out)

	exact := strings.Repeat("a", maxItemBytes)
	out, truncated = capContent(exact)
	assert.False(t, truncated, "a body exactly at the cap is not truncated")
	assert.Equal(t, exact, out)

	over := strings.Repeat("a", maxItemBytes+1)
	out, truncated = capContent(over)
	assert.True(t, truncated)
	assert.Equal(t, maxItemBytes+len(truncationMarker), len(out))
	assert.Contains(t, out, "truncated at the size cap",
		"a reader has to be able to tell a cut item from a short source")
}

func TestDaysBetweenCountsCalendarDays(t *testing.T) {
	// Late yesterday to early today is one day, not zero: the cadence is
	// counted in days, not in elapsed hours.
	assert.Equal(t, 1, daysBetween(at(t, "2026-09-03T23:00:00Z"), at(t, "2026-09-04T01:00:00Z")))
	assert.Equal(t, 0, daysBetween(at(t, "2026-09-04T00:10:00Z"), at(t, "2026-09-04T23:50:00Z")))
	assert.Equal(t, 7, daysBetween(at(t, "2026-09-01T12:00:00Z"), at(t, "2026-09-08T06:00:00Z")))
}

// --- built-in collectors, against a real server ---

func TestFeedCollector(t *testing.T) {
	const rss = `<?xml version="1.0"?>
<rss version="2.0"><channel>
  <title>Example</title>
  <item>
    <title>First post</title>
    <link>https://example.com/first</link>
    <description>a teaser</description>
    <content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">the full body of the first post</content:encoded>
    <pubDate>Thu, 03 Sep 2026 22:00:00 +0000</pubDate>
  </item>
  <item>
    <title>Second post</title>
    <link>https://example.com/second</link>
    <description>only a description here</description>
  </item>
</channel></rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, userAgent, r.Header.Get("User-Agent"),
			"the engine identifies itself so an operator can see who is fetching")
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, rss)
	}))
	defer srv.Close()

	items, err := (&feedCollector{client: srv.Client()}).Collect(context.Background(), source(t, "url: "+srv.URL))
	require.NoError(t, err)
	require.Len(t, items, 2)

	assert.Equal(t, "https://example.com/first", items[0].URL)
	assert.Equal(t, "First post", items[0].Title)
	// The longer of content and description wins, so a feed that puts the
	// article in one and a teaser in the other yields the article.
	assert.Equal(t, "the full body of the first post", items[0].Content)
	assert.Equal(t, at(t, "2026-09-03T22:00:00Z"), items[0].SourceTime.UTC())

	assert.Equal(t, "only a description here", items[1].Content)
	assert.True(t, items[1].SourceTime.IsZero(), "an entry with no date claims none")
}

func TestFeedCollectorParsesAtom(t *testing.T) {
	const atom = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example</title>
  <entry>
    <title>An entry</title>
    <link href="https://example.com/entry"/>
    <updated>2026-09-02T10:00:00Z</updated>
    <content type="text">atom body text</content>
  </entry>
</feed>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, atom)
	}))
	defer srv.Close()

	items, err := (&feedCollector{client: srv.Client()}).Collect(context.Background(), source(t, "url: "+srv.URL))
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "https://example.com/entry", items[0].URL)
	assert.Equal(t, "atom body text", items[0].Content)
	assert.Equal(t, at(t, "2026-09-02T10:00:00Z"), items[0].SourceTime.UTC())
}

func TestFeedCollectorMaxItems(t *testing.T) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Example</title>`)
	for i := range 5 {
		body.WriteString(`<item><title>t</title><link>https://example.com/` + string(rune('a'+i)) + `</link><description>d</description></item>`)
	}
	body.WriteString(`</channel></rss>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body.String())
	}))
	defer srv.Close()

	c := &feedCollector{client: srv.Client()}

	items, err := c.Collect(context.Background(), source(t, "url: "+srv.URL+"\n      max_items: 2"))
	require.NoError(t, err)
	assert.Len(t, items, 2, "a first run against a long archive must not write the whole thing")

	items, err = c.Collect(context.Background(), source(t, "url: "+srv.URL))
	require.NoError(t, err)
	assert.Len(t, items, 5, "no max_items means no limit")
}

func TestFeedCollectorErrors(t *testing.T) {
	t.Run("missing url", func(t *testing.T) {
		_, err := (&feedCollector{client: http.DefaultClient}).Collect(context.Background(), config.Source{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "params.url")
	})

	t.Run("negative max_items", func(t *testing.T) {
		_, err := (&feedCollector{client: http.DefaultClient}).Collect(context.Background(),
			source(t, "url: https://example.com/f\n      max_items: -1"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_items")
	})

	t.Run("non-2xx status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		_, err := (&feedCollector{client: srv.Client()}).Collect(context.Background(), source(t, "url: "+srv.URL))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
	})

	t.Run("body is not a feed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "this is not a feed")
		}))
		defer srv.Close()

		_, err := (&feedCollector{client: srv.Client()}).Collect(context.Background(), source(t, "url: "+srv.URL))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse feed")
	})
}

func TestHTTPCollector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html><body>page text</body></html>")
	}))
	defer srv.Close()

	items, err := (&httpCollector{client: srv.Client()}).Collect(context.Background(), source(t, "url: "+srv.URL))
	require.NoError(t, err)
	require.Len(t, items, 1)

	assert.Equal(t, srv.URL, items[0].URL)
	// Layer 1 is dumb on purpose: the markup is kept exactly as served, and
	// deciding which part of a page is the content is the aggregator's call.
	assert.Equal(t, "<html><body>page text</body></html>", items[0].Content)
}

func TestHTTPCollectorNeedsAURL(t *testing.T) {
	_, err := (&httpCollector{client: http.DefaultClient}).Collect(context.Background(), config.Source{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "params.url")
}

// source builds a config.Source carrying the given params YAML, going through
// the real loader so params decode exactly as they would in production.
func source(t *testing.T, paramsYAML string) config.Source {
	t.Helper()

	cfg := testConfig(t, t.TempDir(), `  a-source:
    collector: feed
    cadence: daily
    params:
      `+paramsYAML+"\n")
	return cfg.Sources["a-source"]
}

// TestFeedCollectorAcceptsAFeedLargerThanOneItem is the regression test for the
// bug this collector shipped with: the raw feed fetch reused the single-item
// byte budget, so a legitimate full-content feed over that size was truncated
// mid-XML and failed to parse — zero items, every pass, with no way to raise it.
// A feed is a whole document holding many entries; only the extracted text of
// each entry needs the per-item bound.
func TestFeedCollectorAcceptsAFeedLargerThanOneItem(t *testing.T) {
	// Each entry is small; the document as a whole clears the per-item cap.
	entryBody := strings.Repeat("x", 40*1024)
	entries := (maxItemBytes / len(entryBody)) + 4

	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Big</title>`)
	for i := range entries {
		body.WriteString(`<item><title>t</title><link>https://example.com/` + strconv.Itoa(i) +
			`</link><description>` + entryBody + `</description></item>`)
	}
	body.WriteString(`</channel></rss>`)
	require.Greater(t, body.Len(), maxItemBytes,
		"the fixture has to exceed the per-item cap or it does not exercise the bug")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body.String())
	}))
	defer srv.Close()

	items, err := (&feedCollector{client: srv.Client()}).Collect(context.Background(), source(t, "url: "+srv.URL))
	require.NoError(t, err)
	assert.Len(t, items, entries)
}

func TestFeedCollectorRejectsAnOversizeFeedClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 5000))
	}))
	defer srv.Close()

	// A small ceiling drives the overflow path without materialising 16 MiB.
	_, err := (&feedCollector{client: srv.Client(), maxBytes: 1000}).Collect(context.Background(), source(t, "url: "+srv.URL))

	require.Error(t, err)
	// A truncated feed fails inside the XML parser with a message that says
	// nothing about the real cause, so the size has to be named here instead.
	assert.Contains(t, err.Error(), "larger than the 1000 byte fetch limit")
	assert.NotContains(t, err.Error(), "parse feed",
		"the size must be reported as the cause, not as a downstream syntax error")
}

func TestHTTPCollectorStillTruncatesAnOversizePage(t *testing.T) {
	// The page path keeps the item cap and the visible truncation marker: a long
	// page is still worth reading, unlike half a feed.
	// Comfortably past the item cap, so a fetch that used the far larger feed
	// ceiling would visibly read more than it should.
	const served = maxItemBytes * 4
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("p", served))
	}))
	defer srv.Close()

	items, err := (&httpCollector{client: srv.Client()}).Collect(context.Background(), source(t, "url: "+srv.URL))
	require.NoError(t, err)
	require.Len(t, items, 1)

	// The bound has to be applied at READ time, not just by capContent
	// afterwards: pulling the whole body into memory first is the thing the
	// limit exists to avoid, and capContent alone cannot tell the two apart.
	assert.LessOrEqual(t, len(items[0].Content), maxItemBytes+1,
		"the page fetch must stop at the item cap rather than reading the whole body")

	capped, truncated := capContent(items[0].Content)
	assert.True(t, truncated, "an oversize page must still reach capContent as oversize")
	assert.Contains(t, capped, "truncated at the size cap")
}

// runPass performs one collection pass at the given instant. Passing the same
// cfg and stub across calls is what lets a test watch the cadence decide, since
// the data root persists between passes and the stub keeps counting its calls.
func runPass(t *testing.T, cfg *config.Config, now time.Time, stub *stubCollector) Result {
	t.Helper()
	return NewRunner(cfg,
		WithClock(func() time.Time { return now }),
		WithCollector("feed", stub),
		WithLogger(quietLogger()),
	).Run(context.Background())
}

// TestEmptyRunIsNotRefetchedUntilTheCadenceElapses is the behaviour #18
// reported and ADR-0004 decides, asserted the way an observer of the cadence
// sees it: whether the source is fetched again, not whether a file appeared.
func TestEmptyRunIsNotRefetchedUntilTheCadenceElapses(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: feed, cadence: weekly}\n")
	day0 := at(t, "2026-09-04T06:30:00Z")

	// The fetch succeeds and legitimately finds nothing.
	stub := &stubCollector{items: nil}
	first := runPass(t, cfg, day0, stub)

	// Vacuity guard: the rest of this test means nothing unless the first pass
	// really was a *successful* fetch that wrote no items.
	require.Len(t, first.Sources, 1)
	require.False(t, first.Sources[0].Skipped, "the source must have been due on the first pass")
	require.NoError(t, first.Sources[0].Err, "the fixture must model a successful fetch, not a failure")
	require.Zero(t, first.Sources[0].Written, "the fixture must model a zero-item run")
	require.Equal(t, 1, stub.calls, "the collector must actually have been called")

	// Every day inside the weekly cadence: not due, and not fetched.
	for back := 1; back <= 6; back++ {
		day := day0.AddDate(0, 0, back)
		result := runPass(t, cfg, day, stub)

		require.Len(t, result.Sources, 1)
		assert.True(t, result.Sources[0].Skipped,
			"a source that ran and found nothing must stay not-due within its cadence (day +%d)", back)
		assert.Equal(t, 1, stub.calls,
			"the source must not be re-fetched inside its cadence (day +%d)", back)
	}

	// And the cadence still fires when it elapses — the marker delays the next
	// fetch, it does not cancel it.
	result := runPass(t, cfg, day0.AddDate(0, 0, 7), stub)
	require.Len(t, result.Sources, 1)
	assert.False(t, result.Sources[0].Skipped, "the source must become due again once the cadence elapses")
	assert.Equal(t, 2, stub.calls)
}

// TestFailedRunIsRetriedOnTheNextPass is the other half of the decision: a
// failure must NOT leave a marker, so its absence keeps meaning "not due yet,
// or broken" and the source is retried rather than written off as empty.
func TestFailedRunIsRetriedOnTheNextPass(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: feed, cadence: weekly}\n")
	day0 := at(t, "2026-09-04T06:30:00Z")

	failing := &stubCollector{err: errors.New("feed is down")}
	first := runPass(t, cfg, day0, failing)

	// Vacuity guard: the fixture must model a failure that wrote nothing.
	require.Len(t, first.Sources, 1)
	require.Error(t, first.Sources[0].Err, "the fixture must model a failed fetch")
	require.Zero(t, first.Sources[0].Written)
	require.Equal(t, 1, failing.calls)

	// No marker may exist for it.
	_, err := os.Stat(filepath.Join(store.New(root).RanDir(day0), "a-source"))
	assert.True(t, os.IsNotExist(err), "a failed fetch must leave no marker")

	// So the very next pass tries again, well inside the weekly cadence.
	second := runPass(t, cfg, day0.AddDate(0, 0, 1), failing)
	require.Len(t, second.Sources, 1)
	assert.False(t, second.Sources[0].Skipped, "a failed source must be retried, not treated as empty")
	assert.Equal(t, 2, failing.calls)
}

// TestEmptyRunMarksOnlyTheSourceThatRan keeps the marker from becoming a
// blanket "the pass happened" flag.
func TestEmptyRunMarksOnlyTheSourceThatRan(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root,
		"  a-source: {collector: feed, cadence: weekly}\n  b-source: {collector: feed, cadence: weekly}\n")
	day0 := at(t, "2026-09-04T06:30:00Z")

	// Both sources share one collector here, so both run and both find nothing.
	stub := &stubCollector{items: nil}
	require.Len(t, runPass(t, cfg, day0, stub).Sources, 2)

	ran := store.New(root).RanDir(day0)
	for _, id := range []string{"a-source", "b-source"} {
		_, err := os.Stat(filepath.Join(ran, id))
		assert.NoError(t, err, "each source that ran empty gets its own marker: %s", id)
	}

	entries, err := os.ReadDir(ran)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "no marker may be written for a source that did not run")
}

// TestSuccessfulRunWithItemsWritesNoMarker — the items themselves prove the
// run, so a marker beside them would be redundant state to keep true.
func TestSuccessfulRunWithItemsWritesNoMarker(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, "  a-source: {collector: feed, cadence: weekly}\n")
	day0 := at(t, "2026-09-04T06:30:00Z")

	stub := &stubCollector{items: []Collected{{URL: "https://example.com/a", Content: "body"}}}
	result := runPass(t, cfg, day0, stub)

	require.Len(t, result.Sources, 1)
	require.NoError(t, result.Sources[0].Err)
	require.Equal(t, 1, result.Sources[0].Written, "vacuity guard: this pass must have written an item")

	_, err := os.Stat(store.New(root).RanDir(day0))
	assert.True(t, os.IsNotExist(err), "a run that produced items needs no marker directory at all")
}

// testConfigRetention builds a config with an explicit item-retention window and
// a single source on the given cadence.
func testConfigRetention(t *testing.T, dataRoot string, items int, cadence config.Cadence) *config.Config {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "roozane.yaml")
	body := "data_root: " + dataRoot + `
relevance_profile: profile.md
retention:
  items: ` + strconv.Itoa(items) + `
  digests: 0
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small, digest: large}
sources:
  a-source: {collector: feed, cadence: ` + string(cadence) + "}\n"

	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err, "a window equal to the cadence must pass validation")
	return cfg
}

// TestPruningCutoffMatchesTheValidationBoundary is the test the two mechanisms
// are pinned to each other by. Config validation accepts retention.items equal
// to the longest cadence on the argument that only the days strictly inside the
// cadence period can change a due-ness answer; pruning has to keep exactly
// those days for that argument to hold.
//
// It composes both sides rather than asserting either in isolation: the window
// is taken from Cadence.Days() — the same quantity validation compares against
// — the config must load, and the pass must then decide "not due" against a
// layout it has just pruned. Move the pruning cutoff a day either way, or
// loosen the validation, and this fails.
func TestPruningCutoffMatchesTheValidationBoundary(t *testing.T) {
	for _, cadence := range []config.Cadence{config.CadenceDaily, config.CadenceWeekly, config.CadenceMonthly} {
		period, ok := cadence.Days()
		require.True(t, ok, "vacuity guard: %q must be a known cadence", cadence)

		t.Run(string(cadence), func(t *testing.T) {
			root := t.TempDir()
			now := at(t, "2026-09-30T06:30:00Z")

			// The smallest window validation accepts for this cadence.
			cfg := testConfigRetention(t, root, period, cadence)

			// The oldest run that must still be readable: one day inside the
			// period. A run older than this makes the source due anyway, so
			// this is the only age where losing the evidence changes anything.
			lastRun := now.AddDate(0, 0, -(period - 1))
			_, err := store.New(root).WriteItem(store.Item{
				Source: "a-source", URL: "https://example.com/seed",
				FetchedAt: lastRun, Collector: "feed", Content: "seed",
			})
			require.NoError(t, err)

			stub := &stubCollector{items: []Collected{{URL: "https://example.com/new", Content: "new"}}}
			result := runPass(t, cfg, now, stub)

			// The pass pruned before it decided, and the deciding day survived.
			require.NoError(t, result.PruneErr)
			assert.DirExists(t, store.New(root).ItemsDir(lastRun),
				"the window must keep the oldest day that can still change the answer")

			require.Len(t, result.Sources, 1)
			assert.True(t, result.Sources[0].Skipped,
				"a source last run inside its cadence must not be due after the prune")
			assert.Zero(t, stub.calls, "and must not be fetched")
		})
	}
}

// TestPruningRemovesTheDayOneStepPastTheBoundary is the other side of the same
// pin: at exactly the cadence period the day is pruned, and the source is due —
// which is the correct answer, and shows the cutoff sits where the comment says.
func TestPruningRemovesTheDayOneStepPastTheBoundary(t *testing.T) {
	cadence := config.CadenceWeekly
	period, ok := cadence.Days()
	require.True(t, ok)

	root := t.TempDir()
	now := at(t, "2026-09-30T06:30:00Z")
	cfg := testConfigRetention(t, root, period, cadence)

	lastRun := now.AddDate(0, 0, -period)
	_, err := store.New(root).WriteItem(store.Item{
		Source: "a-source", URL: "https://example.com/seed",
		FetchedAt: lastRun, Collector: "feed", Content: "seed",
	})
	require.NoError(t, err)

	stub := &stubCollector{items: []Collected{{URL: "https://example.com/new", Content: "new"}}}
	result := runPass(t, cfg, now, stub)

	require.NoError(t, result.PruneErr)
	assert.NoDirExists(t, store.New(root).ItemsDir(lastRun),
		"a day at exactly the window's age is past it")

	require.Len(t, result.Sources, 1)
	assert.False(t, result.Sources[0].Skipped,
		"a source whose cadence has elapsed is due — pruned evidence or not, the answer is the same")
	assert.Equal(t, 1, stub.calls)
}

// TestCollectPassPrunesAndStillCollects — retention failing to run would be bad,
// but retention stopping the collection would be worse.
func TestCollectPassPrunesAndStillCollects(t *testing.T) {
	root := t.TempDir()
	now := at(t, "2026-09-30T06:30:00Z")
	cfg := testConfigRetention(t, root, 7, config.CadenceDaily)

	s := store.New(root)
	_, err := s.WriteItem(store.Item{
		Source: "a-source", URL: "https://example.com/old",
		FetchedAt: now.AddDate(0, 0, -20), Collector: "feed", Content: "old",
	})
	require.NoError(t, err)

	stub := &stubCollector{items: []Collected{{URL: "https://example.com/new", Content: "new"}}}
	result := runPass(t, cfg, now, stub)

	require.NoError(t, result.PruneErr)
	assert.Equal(t, 1, result.Pruned.Days, "the stale day is gone")
	assert.NoDirExists(t, s.DayDir(now.AddDate(0, 0, -20)))

	require.Len(t, result.Sources, 1)
	require.NoError(t, result.Sources[0].Err)
	assert.Equal(t, 1, result.Sources[0].Written, "and the pass still collected")
}
