package collect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mmcdole/gofeed"
	"github.com/yaad-index/roozane/internal/config"
)

// feedParams is what a `collector: feed` source configures.
type feedParams struct {
	URL string `yaml:"url"`

	// MaxItems bounds how many entries are taken from one fetch. Feeds vary
	// wildly in length and a first run against a long archive would otherwise
	// write hundreds of files at once. Zero means no limit.
	MaxItems int `yaml:"max_items"`
}

// feedCollector reads an RSS or Atom feed and returns each entry's text.
type feedCollector struct {
	client *http.Client
}

// Collect fetches and parses the feed. It reports the entries in the order the
// feed lists them, unmodified: choosing which of them matter is the
// aggregator's job, not this layer's.
func (c *feedCollector) Collect(ctx context.Context, src config.Source) ([]Collected, error) {
	var params feedParams
	if err := src.DecodeParams(&params); err != nil {
		return nil, fmt.Errorf("feed params: %w", err)
	}
	if params.URL == "" {
		return nil, errors.New("feed source needs params.url")
	}
	if params.MaxItems < 0 {
		return nil, fmt.Errorf("feed params.max_items must not be negative, got %d", params.MaxItems)
	}

	body, err := fetch(ctx, c.client, params.URL)
	if err != nil {
		return nil, err
	}

	parsed, err := gofeed.NewParser().Parse(strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse feed %s: %w", params.URL, err)
	}

	items := make([]Collected, 0, len(parsed.Items))
	for _, entry := range parsed.Items {
		if entry == nil {
			continue
		}
		item := Collected{
			URL:     entry.Link,
			Title:   entry.Title,
			Content: entryText(entry),
		}
		// A feed's own publication date is provenance. It never decides the day
		// folder — the engine's stamp does (ADR-0003 §2).
		if entry.PublishedParsed != nil {
			item.SourceTime = *entry.PublishedParsed
		} else if entry.UpdatedParsed != nil {
			item.SourceTime = *entry.UpdatedParsed
		}

		items = append(items, item)
		if params.MaxItems > 0 && len(items) == params.MaxItems {
			break
		}
	}

	return items, nil
}

// entryText picks the fullest text a feed entry offers. Feeds disagree about
// which field carries the body: some put the whole article in content and a
// teaser in description, others fill only description. Preferring the longer
// one gets the most text without having to know which convention this feed
// follows.
func entryText(entry *gofeed.Item) string {
	text := entry.Content
	if len(entry.Description) > len(text) {
		text = entry.Description
	}
	return strings.TrimSpace(text)
}

// fetch performs a GET and returns the body as text, bounded by the item cap so
// a hostile or broken endpoint cannot exhaust memory. The cap is applied at
// read time rather than after, because reading it all first is the thing being
// avoided.
func fetch(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request for %s: %w", url, err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body; a close error says nothing actionable

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch %s: unexpected status %s", url, resp.Status)
	}

	// One byte past the cap, so a body that is exactly at the limit is not
	// mistaken for a truncated one.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxItemBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", url, err)
	}
	return string(body), nil
}

// userAgent identifies the engine to the sites it reads. A blank or forged
// agent makes a self-hosted reader look like a scraper to an operator trying to
// work out who is fetching their pages.
const userAgent = "roozane/1 (+https://github.com/yaad-index/roozane)"
