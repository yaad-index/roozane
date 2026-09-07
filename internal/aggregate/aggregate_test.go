package aggregate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/roozane/internal/collect"
	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/llm"
	"github.com/yaad-index/roozane/internal/store"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}

// passOf names which of the three passes a request belongs to, by exact match
// on its system prompt.
//
// Dispatching on the pass rather than on call order is what lets a test say
// "the enrich call" without depending on how many editions ran before it — and
// the model name cannot do this job, since enrich and select deliberately share
// the small model.
func passOf(req llm.Request) string {
	if len(req.Messages) == 0 {
		return "unknown"
	}
	switch req.Messages[0].Content {
	case enrichSystemPrompt:
		return "enrich"
	case selectSystemPrompt:
		return "select"
	case digestSystemPrompt:
		return "digest"
	default:
		return "unknown"
	}
}

// stubClient answers per pass rather than from a flat queue, recording every
// request. Each hook may be nil, in which case a usable default is returned.
type stubClient struct {
	enrich func(req llm.Request) (llm.Response, error)
	sel    func(req llm.Request) (llm.Response, error)
	digest func(req llm.Request) (llm.Response, error)

	calls []llm.Request
}

func (s *stubClient) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	s.calls = append(s.calls, req)

	switch passOf(req) {
	case "enrich":
		if s.enrich != nil {
			return s.enrich(req)
		}
		return llm.Response{Content: enrichJSON("A summary with substance.", 0.8)}, nil
	case "select":
		if s.sel != nil {
			return s.sel(req)
		}
		return llm.Response{Content: selectionJSON(true, 0.9)}, nil
	case "digest":
		if s.digest != nil {
			return s.digest(req)
		}
		return llm.Response{Content: "- the digest body"}, nil
	default:
		return llm.Response{}, errors.New("stub: unrecognised pass")
	}
}

// callsIn returns every recorded request belonging to one pass.
func (s *stubClient) callsIn(pass string) []llm.Request {
	var out []llm.Request
	for _, c := range s.calls {
		if passOf(c) == pass {
			out = append(out, c)
		}
	}
	return out
}

func enrichJSON(summary string, salience float64) string {
	raw, _ := json.Marshal(Enrichment{
		Summary:  summary,
		Points:   []string{"A concrete point."},
		Tags:     []string{"a-tag"},
		Category: "announcement",
		Salience: salience,
	})
	return string(raw)
}

func selectionJSON(selected bool, score float64) string {
	raw, _ := json.Marshal(Selection{Selected: selected, Score: score, Reason: "because"})
	return string(raw)
}

// fixture builds a data root with a profile and the given items already
// collected, plus a loaded config pointing at both. extraYAML is appended to the
// config so a test can add editions.
func fixture(t *testing.T, day time.Time, profile, extraYAML string, items ...store.Item) (*config.Config, string) {
	t.Helper()

	dir := t.TempDir()
	root := filepath.Join(dir, "data")

	profilePath := filepath.Join(dir, "profile.md")
	require.NoError(t, os.WriteFile(profilePath, []byte(profile), 0o600))

	s := store.New(root)
	for _, item := range items {
		item.FetchedAt = day
		if item.Collector == "" {
			item.Collector = "feed"
		}
		_, err := s.WriteItem(item)
		require.NoError(t, err)
	}

	cfgPath := filepath.Join(dir, "roozane.yaml")
	body := "data_root: " + root + "\nrelevance_profile: " + profilePath + `
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small-model, digest: large-model}
sources:
  a-source: {collector: feed, cadence: daily}
  b-source: {collector: feed, cadence: daily}
` + extraYAML
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o600))

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	return cfg, root
}

// writeProfile drops an extra profile beside the config, for editions that own
// one. It returns the absolute path.
func writeProfile(t *testing.T, cfg *config.Config, name, body string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(cfg.RelevanceProfilePath()), name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func runner(t *testing.T, cfg *config.Config, client Completer, now time.Time, opts ...Option) *Runner {
	t.Helper()
	all := append([]Option{
		WithClient(client),
		WithClock(func() time.Time { return now }),
		WithLogger(quietLogger()),
	}, opts...)
	r, err := NewRunner(cfg, all...)
	require.NoError(t, err)
	return r
}

// readDigest loads one edition's written digest.
func readDigest(t *testing.T, root string, day time.Time, edition string) (string, Digest) {
	t.Helper()
	mdPath, jsonPath := store.New(root).DigestPaths(day, edition)

	markdown, err := os.ReadFile(mdPath)
	require.NoError(t, err)

	raw, err := os.ReadFile(jsonPath)
	require.NoError(t, err)
	var digest Digest
	require.NoError(t, json.Unmarshal(raw, &digest))

	return string(markdown), digest
}

func readState(t *testing.T, root string, day time.Time) State {
	t.Helper()
	raw, err := os.ReadFile(store.New(root).StatePath(day))
	require.NoError(t, err)
	var state State
	require.NoError(t, json.Unmarshal(raw, &state))
	return state
}

// --- the neutral pass ---

// TestEnrichCarriesNoAudience is the property the whole two-pass split exists
// for. If the enrichment call could see a profile, its cached result would be
// audience-specific, and serving one audience's reasoning to another is the
// leak ADR-0005 removes by construction rather than defends against.
func TestEnrichCarriesNoAudience(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	const secret = "I care intensely about EXAMPLE-PRIVATE-INTEREST"
	cfg, _ := fixture(t, day, secret, "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "A", Content: "body"})

	client := &stubClient{}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	enrichCalls := client.callsIn("enrich")
	require.Len(t, enrichCalls, 1)
	for _, msg := range enrichCalls[0].Messages {
		assert.NotContains(t, msg.Content, "EXAMPLE-PRIVATE-INTEREST",
			"the enrichment call must not carry the reader's profile: its result is cached and served to every edition")
	}

	// The selection call is where the profile belongs, so the test also proves
	// the profile was in play at all rather than simply absent everywhere.
	selectCalls := client.callsIn("select")
	require.Len(t, selectCalls, 1)
	assert.Contains(t, selectCalls[0].Messages[1].Content, "EXAMPLE-PRIVATE-INTEREST")
}

// TestSelectionReadsTheSummaryNotTheItem is what makes selection cheaper than
// the per-reader judgement it replaces.
func TestSelectionReadsTheSummaryNotTheItem(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", "",
		store.Item{
			Source: "a-source", URL: "https://example.com/a", Title: "A",
			Content: "FULL-ITEM-BODY-MARKER that selection should never see",
		})

	client := &stubClient{
		enrich: func(llm.Request) (llm.Response, error) {
			return llm.Response{Content: enrichJSON("SUMMARY-MARKER", 0.7)}, nil
		},
	}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	selectCalls := client.callsIn("select")
	require.Len(t, selectCalls, 1)
	body := selectCalls[0].Messages[1].Content
	assert.Contains(t, body, "SUMMARY-MARKER")
	assert.NotContains(t, body, "FULL-ITEM-BODY-MARKER",
		"selection is given the enriched summary in place of the item")
}

func TestRunWritesBothDigestFilesUnderTheEdition(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "A", Content: "body"})

	client := &stubClient{}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Items)
	assert.Equal(t, 1, result.Enriched)
	require.Len(t, result.Editions, 1)
	assert.Equal(t, config.DefaultEdition, result.Editions[0].ID)
	assert.Equal(t, 1, result.Editions[0].Selected)

	markdown, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Contains(t, markdown, "the digest body")
	assert.Equal(t, DigestSchema, digest.Schema)
	assert.Equal(t, config.DefaultEdition, digest.Edition,
		"a digest names its edition, because a sink receives the document and not its path")
	assert.False(t, digest.Empty)
	require.Len(t, digest.Items, 1)
	assert.Equal(t, "a-source", digest.Items[0].Source)
	assert.Equal(t, []string{"A concrete point."}, digest.Items[0].Points, "points come from the neutral pass")
	assert.Equal(t, "announcement", digest.Items[0].Category)
	assert.InDelta(t, 0.9, digest.Items[0].Score, 0.0001, "the score is the edition's, not the item's salience")
}

func TestQuietEditionWritesAnExplicitEmptyDigest(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	client := &stubClient{
		sel: func(llm.Request) (llm.Response, error) {
			return llm.Response{Content: selectionJSON(false, 0.1)}, nil
		},
	}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	require.Len(t, result.Editions, 1)
	assert.True(t, result.Editions[0].Empty)

	markdown, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Contains(t, markdown, emptyDigestMarker)
	assert.True(t, digest.Empty)
	assert.Empty(t, digest.Items)

	// A quiet edition must not pay for a writing call: there is nothing to
	// write from, and asking for prose about nothing is how filler appears.
	assert.Empty(t, client.callsIn("digest"))
}

func TestNoItemsStillWritesAnEmptyDigest(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")

	client := &stubClient{}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Zero(t, result.Items)
	require.Len(t, result.Editions, 1)
	assert.True(t, result.Editions[0].Empty)

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.True(t, digest.Empty)
	assert.Empty(t, client.calls, "no items means no calls at all")
}

// --- editions ---

const twoEditions = `
editions:
  personal: {}
  boardgames: {sources: [b-source]}
`

func TestEachEditionGetsItsOwnDigest(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", twoEditions,
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "A", Content: "body a"},
		store.Item{Source: "b-source", URL: "https://example.com/b", Title: "B", Content: "body b"})

	client := &stubClient{}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	// Each item is read once regardless of how many editions want it: that is
	// the whole economy of the neutral pass.
	assert.Len(t, client.callsIn("enrich"), 2)
	assert.Equal(t, 2, result.Enriched)

	require.Len(t, result.Editions, 2)
	byID := map[string]EditionResult{}
	for _, e := range result.Editions {
		byID[e.ID] = e
	}

	assert.Equal(t, 2, byID["personal"].Candidates, "an edition with no source list draws on the whole pool")
	assert.Equal(t, 1, byID["boardgames"].Candidates, "a source list narrows the pool first")

	_, personal := readDigest(t, root, day, "personal")
	_, boardgames := readDigest(t, root, day, "boardgames")
	assert.Equal(t, "personal", personal.Edition)
	assert.Equal(t, "boardgames", boardgames.Edition)
	assert.Len(t, personal.Items, 2)
	require.Len(t, boardgames.Items, 1)
	assert.Equal(t, "b-source", boardgames.Items[0].Source)
}

// TestAnItemCanAppearInTwoEditions is the requirement that ruled out routing an
// item to one destination: the outputs are not a partition.
func TestAnItemCanAppearInTwoEditions(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", twoEditions,
		store.Item{Source: "b-source", URL: "https://example.com/b", Title: "B", Content: "body b"})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, personal := readDigest(t, root, day, "personal")
	_, boardgames := readDigest(t, root, day, "boardgames")
	require.Len(t, personal.Items, 1)
	require.Len(t, boardgames.Items, 1)
	assert.Equal(t, personal.Items[0].URL, boardgames.Items[0].URL)
}

// TestAnEmptySourceListSelectsNothing is the parked edition. It is the opposite
// of an omitted list, and the two must not collapse into each other.
func TestAnEmptySourceListSelectsNothing(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "\neditions:\n  parked: {sources: []}\n  open: {}\n",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	client := &stubClient{}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	byID := map[string]EditionResult{}
	for _, e := range result.Editions {
		byID[e.ID] = e
	}
	assert.Zero(t, byID["parked"].Candidates)
	assert.Equal(t, 1, byID["open"].Candidates)

	// A parked edition still proves it ran.
	_, parked := readDigest(t, root, day, "parked")
	assert.True(t, parked.Empty)
}

func TestAnEditionUsesItsOwnProfile(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "TOP-LEVEL-PROFILE", "", store.Item{Source: "a-source", Content: "body"})

	own := writeProfile(t, cfg, "boardgames.md", "OWN-PROFILE")
	cfg.Editions = map[string]config.Edition{
		"inherits": {Profile: cfg.RelevanceProfile},
		"owns-one": {Profile: own},
	}

	client := &stubClient{}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	var sawTopLevel, sawOwn bool
	for _, call := range client.callsIn("select") {
		body := call.Messages[1].Content
		sawTopLevel = sawTopLevel || strings.Contains(body, "TOP-LEVEL-PROFILE")
		sawOwn = sawOwn || strings.Contains(body, "OWN-PROFILE")
	}
	assert.True(t, sawTopLevel, "the inheriting edition selects against the top-level profile")
	assert.True(t, sawOwn, "the edition with its own profile selects against that one")
}

// TestOneFailingEditionDoesNotCostTheOthers keeps a broken profile from taking
// down every audience.
func TestOneFailingEditionDoesNotCostTheOthers(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	cfg.Editions = map[string]config.Edition{
		"broken": {Profile: filepath.Join(t.TempDir(), "absent.md")},
		"works":  {Profile: cfg.RelevanceProfile},
	}

	result, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.Error(t, err, "the run reports the failure")
	assert.Contains(t, err.Error(), `edition "broken"`)

	// The working edition still has its digest on disk.
	_, works := readDigest(t, root, day, "works")
	assert.Len(t, works.Items, 1)
	assert.Equal(t, 2, len(result.Editions), "both editions are reported, including the one that failed")
}

func TestEditionsRunInAStableOrder(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", "\neditions:\n  zulu: {}\n  alpha: {}\n  mike: {}\n",
		store.Item{Source: "a-source", Content: "body"})

	result, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	ids := make([]string, 0, len(result.Editions))
	for _, e := range result.Editions {
		ids = append(ids, e.ID)
	}
	assert.Equal(t, []string{"alpha", "mike", "zulu"}, ids,
		"map iteration is random; two runs over one day must do the same things in the same order")
}

// --- the resume cache ---

func TestReRunReusesRecordedEnrichment(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	first := &stubClient{}
	_, err := runner(t, cfg, first, day).Run(context.Background(), day)
	require.NoError(t, err)
	require.Len(t, first.callsIn("enrich"), 1)

	second := &stubClient{}
	result, err := runner(t, cfg, second, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Empty(t, second.callsIn("enrich"), "an already-enriched item is not read again")
	assert.Equal(t, 1, result.Reused)
	assert.NotEmpty(t, second.callsIn("select"), "selection is not cached: it is per edition")
}

// TestChangedModelInvalidatesTheEnrichment and its prompt-version sibling are
// the two halves of the same rule: the cache key is the item, but the cache's
// validity depends on what produced the record.
func TestChangedModelInvalidatesTheEnrichment(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	first := &stubClient{}
	_, err := runner(t, cfg, first, day).Run(context.Background(), day)
	require.NoError(t, err)

	cfg.Aggregator.Models.Item = "a-different-model"

	second := &stubClient{}
	result, err := runner(t, cfg, second, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Len(t, second.callsIn("enrich"), 1, "a different model must re-read the item")
	assert.Zero(t, result.Reused)
}

func TestChangedPromptVersionInvalidatesTheEnrichment(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	first := &stubClient{}
	_, err := runner(t, cfg, first, day).Run(context.Background(), day)
	require.NoError(t, err)

	// Rewrite the recorded prompt version to an older one, which is what a
	// state file written before a prompt edit looks like.
	state := readState(t, root, day)
	require.Len(t, state.Items, 1)
	for name, item := range state.Items {
		item.PromptVersion = enrichPromptVersion - 1
		state.Items[name] = item
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	require.NoError(t, err)
	require.NoError(t, store.New(root).WriteAtomic(store.New(root).StatePath(day), raw))

	second := &stubClient{}
	result, err := runner(t, cfg, second, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Len(t, second.callsIn("enrich"), 1,
		"a record from an older prompt is not served: nothing in the config would look different")
	assert.Zero(t, result.Reused)
}

func TestStateFromADifferentSchemaIsNotReused(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	s := store.New(root)
	stale := `{"schema":1,"day":"2026-09-04","items":{"a-source--x.md":{"status":"relevant","model":"small-model"}}}`
	require.NoError(t, s.WriteAtomic(s.StatePath(day), []byte(stale)))

	client := &stubClient{}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Len(t, client.callsIn("enrich"), 1)
	assert.Zero(t, result.Reused, "another schema's record may mean something different; the day starts fresh")

	assert.Equal(t, stateSchema, readState(t, root, day).Schema)
}

func TestAFailureIsNeverReusedAsAnEnrichment(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	failing := &stubClient{
		enrich: func(llm.Request) (llm.Response, error) {
			return llm.Response{}, errors.New("upstream is down")
		},
	}
	result, err := runner(t, cfg, failing, day).Run(context.Background(), day)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Failed)

	state := readState(t, root, day)
	require.Len(t, state.Items, 1)
	for _, item := range state.Items {
		assert.Equal(t, StatusFailed, item.Status)
		assert.Nil(t, item.Enrichment)
	}

	// The next run retries it rather than treating the failure as an answer.
	second := &stubClient{}
	result, err = runner(t, cfg, second, day).Run(context.Background(), day)
	require.NoError(t, err)
	assert.Len(t, second.callsIn("enrich"), 1)
	assert.Zero(t, result.Failed)
	assert.Zero(t, result.Reused)
}

// TestOneFailingItemDoesNotCostTheDigest keeps one unreadable item from costing
// the reader everything else that day.
func TestOneFailingItemDoesNotCostTheDigest(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body a"},
		store.Item{Source: "b-source", URL: "https://example.com/b", Content: "body b"})

	var enrichCalls int
	client := &stubClient{
		enrich: func(llm.Request) (llm.Response, error) {
			enrichCalls++
			if enrichCalls == 1 {
				return llm.Response{}, errors.New("upstream is down")
			}
			return llm.Response{Content: enrichJSON("fine", 0.8)}, nil
		},
	}

	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Failed)
	assert.Equal(t, 1, result.Enriched)

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Len(t, digest.Items, 1, "the surviving item still reaches the digest")
}

// --- the salience floor ---

// TestSalienceFloorHoldsItemsBackFromEveryEdition exercises the mechanism while
// the shipped constant is 0. The floor is applied once, before any edition sees
// the pool, which is the only place that saves work per audience rather than
// per audience-item.
func TestSalienceFloorHoldsItemsBackFromEveryEdition(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", twoEditions,
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "junk"},
		store.Item{Source: "b-source", URL: "https://example.com/b", Content: "real"})

	client := &stubClient{
		enrich: func(req llm.Request) (llm.Response, error) {
			if strings.Contains(req.Messages[1].Content, "junk") {
				return llm.Response{Content: enrichJSON("navigation chrome", 0.05)}, nil
			}
			return llm.Response{Content: enrichJSON("real substance", 0.75)}, nil
		},
	}

	result, err := runner(t, cfg, client, day, WithSalienceFloor(0.5)).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Equal(t, 2, result.Enriched, "both items were read: the floor is applied after enrichment")
	assert.Equal(t, 1, result.BelowFloor)

	// The held-back item reaches no edition at all, and is not merely absent
	// from the one whose sources happened to exclude it.
	byID := map[string]EditionResult{}
	for _, e := range result.Editions {
		byID[e.ID] = e
	}
	assert.Equal(t, 1, byID["personal"].Candidates,
		"the whole-pool edition sees one candidate, not two")

	_, personal := readDigest(t, root, day, "personal")
	require.Len(t, personal.Items, 1)
	assert.Equal(t, "b-source", personal.Items[0].Source)
}

// TestTheShippedFloorDropsNothing pins the deliberate conservative default. If
// someone sets a real number without measuring it, this test is what tells
// them they changed behaviour.
func TestTheShippedFloorDropsNothing(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	client := &stubClient{
		enrich: func(llm.Request) (llm.Response, error) {
			// The bottom of the scale: no substance at all.
			return llm.Response{Content: enrichJSON("nothing here", 0)}, nil
		},
	}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Zero(t, result.BelowFloor,
		"the shipped floor is 0 and is meant to drop nothing until it has been measured against a real corpus")
	assert.Equal(t, 1, result.Editions[0].Candidates)
}

// --- models, prompts and parsing ---

func TestPassesUseTheirConfiguredModels(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	client := &stubClient{}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	require.Len(t, client.callsIn("enrich"), 1)
	require.Len(t, client.callsIn("select"), 1)
	require.Len(t, client.callsIn("digest"), 1)

	// Cost scales with the size of the task: reading and selecting use the
	// small model, writing the day's prose uses the larger one.
	assert.Equal(t, "small-model", client.callsIn("enrich")[0].Model)
	assert.Equal(t, "small-model", client.callsIn("select")[0].Model)
	assert.Equal(t, "large-model", client.callsIn("digest")[0].Model)
}

// TestSuppressionIsTheDefaultInTheSelectPrompt guards the product law that used
// to live in the item prompt. It moved with the pass; it did not go away.
func TestSuppressionIsTheDefaultInTheSelectPrompt(t *testing.T) {
	assert.Contains(t, selectSystemPrompt, "SUPPRESSION IS THE DEFAULT")
	assert.Contains(t, selectSystemPrompt, "correct and expected outcome")

	// And the neutral pass must not be asked for relevance at all, which is
	// what keeps its result reusable across audiences.
	assert.NotContains(t, enrichSystemPrompt, "relevance profile")
	assert.Contains(t, enrichSystemPrompt, "YOU DO NOT KNOW WHO WILL READ THIS")
}

// TestEnrichPromptDefinesTheSalienceScale keeps the number comparable to
// something. A floor and a report are both stated in terms of this scale, and a
// score on an undefined scale is not a measurement.
func TestEnrichPromptDefinesTheSalienceScale(t *testing.T) {
	assert.Contains(t, enrichSystemPrompt, "0.0 —")
	assert.Contains(t, enrichSystemPrompt, "1.0 —")
	assert.Contains(t, enrichSystemPrompt, "NOT how interesting")
}

func TestParseEnrichment(t *testing.T) {
	t.Run("plain json", func(t *testing.T) {
		got, err := parseEnrichment(`{"summary":"s","salience":0.4,"tags":["t"],"category":"c"}`)
		require.NoError(t, err)
		assert.Equal(t, "s", got.Summary)
		assert.InDelta(t, 0.4, got.Salience, 0.0001)
	})

	t.Run("fenced json is unwrapped rather than rejected", func(t *testing.T) {
		got, err := parseEnrichment("```json\n{\"summary\":\"s\",\"salience\":0.4}\n```")
		require.NoError(t, err)
		assert.Equal(t, "s", got.Summary)
	})

	t.Run("no summary is rejected", func(t *testing.T) {
		_, err := parseEnrichment(`{"summary":"   ","salience":0.4}`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no summary")
	})

	t.Run("salience off the scale is rejected, not clamped", func(t *testing.T) {
		for _, body := range []string{`{"summary":"s","salience":1.4}`, `{"summary":"s","salience":-0.2}`} {
			_, err := parseEnrichment(body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "outside the 0.0-1.0 scale")
		}
	})

	t.Run("prose is rejected", func(t *testing.T) {
		_, err := parseEnrichment("I think this item is quite interesting.")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "usable JSON")
	})
}

func TestParseSelection(t *testing.T) {
	got, err := parseSelection("```\n{\"selected\":true,\"score\":0.5,\"reason\":\"r\"}\n```")
	require.NoError(t, err)
	assert.True(t, got.Selected)
	assert.InDelta(t, 0.5, got.Score, 0.0001)

	_, err = parseSelection("not json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usable JSON")
}

func TestEmptyProfileIsRejected(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "   \n\n", "", store.Item{Source: "a-source", Content: "body"})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is empty")
}

func TestDigestFilesAreWrittenAtomically(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	entries, err := os.ReadDir(store.New(root).EditionDir(config.DefaultEdition))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "a partially written digest must never be visible under its final name")
	}
	assert.Len(t, entries, 2)
}

// TestEnrichmentIsSavedBeforeSelectionRuns is what stops one broken profile
// from making every item be read again on the next pass.
func TestEnrichmentIsSavedBeforeSelectionRuns(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	cfg.Editions = map[string]config.Edition{
		"broken": {Profile: filepath.Join(t.TempDir(), "absent.md")},
	}

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.Error(t, err)

	// The enrichment was paid for, so it is on disk even though the run failed.
	state := readState(t, root, day)
	require.Len(t, state.Items, 1)
	for _, item := range state.Items {
		assert.Equal(t, StatusEnriched, item.Status)
		require.NotNil(t, item.Enrichment)
	}
}

// --- empty digests carry collection outcomes (ADR-0005 §8) ---

// writeCollected drops a collection record for the day, as the collector would.
func writeCollected(t *testing.T, root string, day time.Time, sources map[string]collect.SourceOutcome) {
	t.Helper()
	raw, err := json.Marshal(collect.Outcomes{
		Schema:  1,
		Day:     store.Day(day),
		Sources: sources,
	})
	require.NoError(t, err)
	s := store.New(root)
	require.NoError(t, s.WriteAtomic(s.CollectedPath(day), raw))
}

// TestAnEmptyDigestNamesItsSilentSources is the confusion §8 exists to prevent:
// a narrow edition whose only source was down produces exactly the same digest
// as one whose source simply had nothing to say.
func TestAnEmptyDigestNamesItsSilentSources(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "\neditions:\n  narrow: {sources: [a-source]}\n")

	writeCollected(t, root, day, map[string]collect.SourceOutcome{
		"a-source": {Ran: true, Items: 0, Error: "dial tcp 10.0.0.5:443: connect: connection refused"},
		"b-source": {Ran: true, Items: 12},
	})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, digest := readDigest(t, root, day, "narrow")
	require.True(t, digest.Empty)

	// The markdown says a selected source produced nothing...
	assert.Contains(t, markdown, "a-source")
	assert.Contains(t, markdown, "No items were collected today")

	// ...and never the raw error. This markdown is what a public newsletter
	// delivers; it must not end with a fetch exception naming an internal host.
	assert.NotContains(t, markdown, "connection refused")
	assert.NotContains(t, markdown, "10.0.0.5")

	// The structured file carries the error, for sinks and tooling.
	require.Contains(t, digest.Sources, "a-source")
	assert.Contains(t, digest.Sources["a-source"].Error, "connection refused")

	// An edition reports only its own sources: naming another edition's feed
	// would be reporting on somebody else's pipeline.
	assert.NotContains(t, digest.Sources, "b-source")
	assert.NotContains(t, markdown, "b-source")
}

// TestAnEditionOverTheWholePoolReportsEverySource is the other half of the
// narrowing: no source list means every configured source is in scope.
func TestAnEditionOverTheWholePoolReportsEverySource(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")

	writeCollected(t, root, day, map[string]collect.SourceOutcome{
		"a-source": {Ran: true, Items: 0},
		"b-source": {Ran: false},
	})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Len(t, digest.Sources, 2)
	assert.Contains(t, markdown, "a-source, b-source")
}

// TestASourceThatProducedItemsIsNotCalledSilent keeps the note about what
// actually failed rather than listing every source in scope.
func TestASourceThatProducedItemsIsNotCalledSilent(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")

	writeCollected(t, root, day, map[string]collect.SourceOutcome{
		"a-source": {Ran: true, Items: 4},
		"b-source": {Ran: true, Items: 0},
	})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, _ := readDigest(t, root, day, config.DefaultEdition)
	assert.Contains(t, markdown, "b-source")
	assert.NotContains(t, markdown, "a-source",
		"a source that produced items is not one that produced nothing")
	assert.Contains(t, markdown, "1 selected source", "the count follows the list")
}

// TestANonEmptyDigestStillCarriesOutcomesStructurally keeps the JSON shape
// uniform, so a reader never has to treat the field's absence as a signal.
func TestANonEmptyDigestStillCarriesOutcomesStructurally(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	writeCollected(t, root, day, map[string]collect.SourceOutcome{
		"a-source": {Ran: true, Items: 1},
		"b-source": {Ran: true, Items: 0, Error: "boom"},
	})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, digest := readDigest(t, root, day, config.DefaultEdition)
	require.False(t, digest.Empty)
	assert.Len(t, digest.Sources, 2, "the structured file carries outcomes whether or not the digest is empty")

	// The note belongs to the empty case: a digest with items is not ambiguous,
	// and a delivered newsletter should not carry engine telemetry in its prose.
	assert.NotContains(t, markdown, "No items were collected today")
}

// TestAMissingCollectionRecordIsNotAnError covers a day collected by an older
// build. "Not known" is honest; a fabricated all-clear would not be.
func TestAMissingCollectionRecordIsNotAnError(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.True(t, digest.Empty)
	assert.Empty(t, digest.Sources)
	assert.NotContains(t, markdown, "No items were collected today",
		"with no record, the digest claims nothing about collection either way")
}

func TestDigestSchemaIsCurrent(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Equal(t, 2, digest.Schema,
		"the digest JSON gained an edition and collection outcomes; the version has to move with the shape")
}
