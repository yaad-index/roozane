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
