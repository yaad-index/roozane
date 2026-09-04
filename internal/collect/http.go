package collect

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/yaad-index/roozane/internal/config"
)

// httpParams is what a `collector: http` source configures.
type httpParams struct {
	URL string `yaml:"url"`
}

// httpCollector fetches one URL and stores the response body as-is.
//
// It deliberately does not strip markup or extract an article: ADR-0001 keeps
// layer 1 dumb, and "which part of this page is the content" is a judgement.
// The aggregator reads the raw text and decides.
type httpCollector struct {
	client *http.Client
}

// Collect fetches the configured URL. One source, one item — the page is the
// item, and its identity is the URL, so re-running within a day rewrites that
// day's single file rather than appending a duplicate. Each day still keeps its
// own snapshot, which is what makes "what did this page say on the 4th" a
// question the layout can answer.
func (c *httpCollector) Collect(ctx context.Context, src config.Source) ([]Collected, error) {
	var params httpParams
	if err := src.DecodeParams(&params); err != nil {
		return nil, fmt.Errorf("http params: %w", err)
	}
	if params.URL == "" {
		return nil, errors.New("http source needs params.url")
	}

	body, err := fetch(ctx, c.client, params.URL)
	if err != nil {
		return nil, err
	}

	return []Collected{{
		URL:     params.URL,
		Content: body,
	}}, nil
}
