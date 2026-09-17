package aggregate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// The digest is matched on its prefix because an edition naming a language
	// appends a rule to the instructions. The others are matched exactly, so a
	// pass that quietly grew a suffix would show up here as "unknown" rather
	// than being waved through.
	switch system := req.Messages[0].Content; {
	case system == enrichSystemPrompt:
		return "enrich"
	case system == selectSystemPrompt:
		return "select"
	case system == groupSystemPrompt:
		return "group"
	case system == titleSystemPrompt:
		return "title"
	case strings.HasPrefix(system, digestSystemPrompt):
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
	group  func(req llm.Request) (llm.Response, error)
	title  func(req llm.Request) (llm.Response, error)
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
	case "group":
		if s.group != nil {
			return s.group(req)
		}
		// One subject holding everything: a valid partition that the ceiling
		// deliberately does not act on, so a test enabling a share without
		// caring about grouping keeps the digest it would otherwise have had.
		return llm.Response{Content: oneSubjectJSON(itemsAskedAbout(req))}, nil
	case "title":
		if s.title != nil {
			return s.title(req)
		}
		// Every headline already in the reader's language, which is the answer
		// that changes nothing.
		return llm.Response{Content: `{"titles": []}`}, nil
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

// TestOneEventIsOneEntryInTheDigestPrompt guards the rule against being softened
// back into the one it replaced.
//
// The NotContains half is the load-bearing one. The prompt previously said "a
// flat list is fine and usually better", and four articles about a single
// municipal data breach became the digest's four leading rows — behaviour that
// was instructed rather than accidental, and therefore invisible to anyone
// reading only the output.
func TestOneEventIsOneEntryInTheDigestPrompt(t *testing.T) {
	assert.NotContains(t, digestSystemPrompt, "flat list is fine and usually better",
		"the instruction that produced one entry per article must not come back")
	assert.Contains(t, digestSystemPrompt, "ONE EVENT IS ONE ENTRY")

	// Sameness has to be judged on substance: the headlines of one story
	// routinely share almost no vocabulary, so a wording test does not work.
	assert.Contains(t, digestSystemPrompt, "not from wording")

	// And the rule has to fail toward keeping items apart, since an unwanted
	// merge loses a story while a missed one only repeats.
	assert.Contains(t, digestSystemPrompt, "when in doubt keep them separate")
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
	assert.Equal(t, 5, digest.Schema,
		"the digest JSON gained the reason its subject-share ceiling could not be applied, "+
			"after a per-item translated title and a title-pass failure, an unjudged count, "+
			"an edition and collection outcomes; the version has to move with the shape")
}

// --- the daily report (ADR-0005 §7) ---

func readReport(t *testing.T, root string, day time.Time) (string, Report) {
	t.Helper()
	mdPath, jsonPath := store.New(root).ReportPaths(day)

	markdown, err := os.ReadFile(mdPath)
	require.NoError(t, err)

	raw, err := os.ReadFile(jsonPath)
	require.NoError(t, err)
	var report Report
	require.NoError(t, json.Unmarshal(raw, &report))

	return string(markdown), report
}

// TestReportSchemaIsCurrent pins the literal, as the digest's equivalent does.
//
// The report's schema is not an internal detail: a sink says either
// `edition: <id>` or `report: true`, and `report: true` composes with `command`,
// so the report's structured JSON is handed to an arbitrary external program on
// stdin under the ADR-0003 contract — schema field included. A silent shape
// change there breaks a reader outside this repo for the same reason the
// digest's would, and it does so while its operator is reading the report to
// diagnose a bad run.
//
// Asserting against the constant cannot serve this purpose: both sides move on
// a bump. Only a literal fails, which is the whole point of writing one.
//
// What a pin guarantees is narrower than it can look. It does not prevent a
// bump, and it does not check that one was correct — a commit moving the
// constant and this literal together passes, and is meant to. It forces the
// bump to appear in the diff as a deliberate edit, and that is the whole of it.
func TestReportSchemaIsCurrent(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)
	assert.Equal(t, 3, report.Schema,
		"an edition now records what its title pass offered, got back and changed; the "+
			"version has to move with the shape, and a reader outside this repo is told "+
			"by this number what to expect")
}

func TestReportRecordsSourcesItemsAndEditions(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "\neditions:\n  narrow: {sources: [a-source]}\n",
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "A", Content: "body a"},
		store.Item{Source: "b-source", URL: "https://example.com/b", Title: "B", Content: "body b"})

	writeCollected(t, root, day, map[string]collect.SourceOutcome{
		"a-source": {Ran: true, Items: 1},
		"b-source": {Ran: true, Items: 1, Error: "flaky upstream"},
	})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, report := readReport(t, root, day)
	// Against the constant, so this says the field is populated and survives the
	// round trip to JSON and back — it is NOT a currency check and cannot fail
	// on a bump, because both sides move together. TestReportSchemaIsCurrent
	// pins the literal and is the test that notices.
	assert.Equal(t, ReportSchema, report.Schema)
	assert.Equal(t, store.Day(day), report.Day)

	// Per source, from the collection record.
	assert.Len(t, report.Sources, 2)
	assert.Contains(t, markdown, "flaky upstream",
		"the report is for the operator, so it carries the error text the digest markdown must not")

	// Per item: tags, category, salience.
	require.Len(t, report.Items, 2)
	assert.Equal(t, StatusEnriched, report.Items[0].Status)
	assert.Equal(t, "announcement", report.Items[0].Category)
	assert.InDelta(t, 0.8, report.Items[0].Salience, 0.0001)

	// Per edition: what it selected, and why each enriched item is absent.
	require.Len(t, report.Editions, 1)
	edition := report.Editions[0]
	assert.Equal(t, "narrow", edition.ID)
	assert.Len(t, edition.Selected, 1)
	require.Len(t, edition.Absent, 1)
	assert.Equal(t, ReasonNotInSources, edition.Absent[0].Reason,
		"the b-source item is absent because this edition's source list excluded it")
}

func TestReportNamesWhyAnItemWasNotSelected(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	client := &stubClient{
		sel: func(llm.Request) (llm.Response, error) {
			return llm.Response{Content: selectionJSON(false, 0.1)}, nil
		},
	}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)
	require.Len(t, report.Editions, 1)
	require.Len(t, report.Editions[0].Absent, 1)
	assert.Equal(t, ReasonNotSelected, report.Editions[0].Absent[0].Reason)
}

func TestReportNamesTheSalienceFloorAsAnAbsenceReason(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "junk"})

	client := &stubClient{
		enrich: func(llm.Request) (llm.Response, error) {
			return llm.Response{Content: enrichJSON("navigation chrome", 0.05)}, nil
		},
	}
	_, err := runner(t, cfg, client, day, WithSalienceFloor(0.5)).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)
	require.Len(t, report.Editions, 1)
	require.Len(t, report.Editions[0].Absent, 1)
	assert.Equal(t, ReasonBelowFloor, report.Editions[0].Absent[0].Reason,
		"an item the floor held back is absent from the edition, and the report says so rather than "+
			"leaving the reader to know the floor runs earlier")
}

func TestReportRecordsEnrichmentFailures(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	client := &stubClient{
		enrich: func(llm.Request) (llm.Response, error) {
			return llm.Response{}, errors.New("upstream is down")
		},
	}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, report := readReport(t, root, day)
	require.Len(t, report.Items, 1)
	assert.Equal(t, StatusFailed, report.Items[0].Status)
	assert.Contains(t, report.Items[0].Error, "upstream is down")
	assert.Contains(t, markdown, "ENRICHMENT FAILED")
}

// TestReportSpendIsPerPassPerModel is the breakdown ADR-0005 §7 asks for.
func TestReportSpendIsPerPassPerModel(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	client := &stubClient{
		enrich: func(llm.Request) (llm.Response, error) {
			return llm.Response{
				Content: enrichJSON("summary", 0.8),
				Usage:   llm.Usage{PromptTokens: 100, CompletionTokens: 10},
			}, nil
		},
		sel: func(llm.Request) (llm.Response, error) {
			return llm.Response{
				Content: selectionJSON(true, 0.9),
				Usage:   llm.Usage{PromptTokens: 20, CompletionTokens: 5},
			}, nil
		},
		digest: func(llm.Request) (llm.Response, error) {
			return llm.Response{
				Content: "- body",
				Usage:   llm.Usage{PromptTokens: 50, CompletionTokens: 200},
			}, nil
		},
	}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)
	byPass := map[string]PassSpend{}
	for _, spend := range report.Spend {
		byPass[spend.Pass] = spend
	}

	require.Contains(t, byPass, PassEnrich)
	assert.Equal(t, "small-model", byPass[PassEnrich].Model)
	assert.Equal(t, 100, byPass[PassEnrich].PromptTokens)
	assert.Equal(t, 10, byPass[PassEnrich].CompletionTokens,
		"prompt and completion are kept apart, because every provider charges them differently")

	require.Contains(t, byPass, PassSelect)
	assert.Equal(t, "small-model", byPass[PassSelect].Model)
	assert.Equal(t, 20, byPass[PassSelect].PromptTokens)

	require.Contains(t, byPass, PassDigest)
	assert.Equal(t, "large-model", byPass[PassDigest].Model,
		"enrich and select share the small model; only the writing pass uses the large one")
	assert.Equal(t, 200, byPass[PassDigest].CompletionTokens)

	assert.True(t, report.SpendIsPerRun)
}

// TestReRunReportsNearZeroSpend is the consequence worth stating in the output,
// because it reads like a bug: a reused enrichment is not paid for again and so
// is not counted.
func TestReRunReportsNearZeroSpend(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	usage := llm.Usage{PromptTokens: 100, CompletionTokens: 10}
	withUsage := func() *stubClient {
		return &stubClient{
			enrich: func(llm.Request) (llm.Response, error) {
				return llm.Response{Content: enrichJSON("summary", 0.8), Usage: usage}, nil
			},
		}
	}

	_, err := runner(t, cfg, withUsage(), day).Run(context.Background(), day)
	require.NoError(t, err)
	_, first := readReport(t, root, day)

	byPass := func(report Report, pass string) (PassSpend, bool) {
		for _, spend := range report.Spend {
			if spend.Pass == pass {
				return spend, true
			}
		}
		return PassSpend{}, false
	}
	enrichSpend, ok := byPass(first, PassEnrich)
	require.True(t, ok)
	assert.Equal(t, 100, enrichSpend.PromptTokens)

	// Second run over the same day: the enrichment is reused, so nothing is
	// paid for and nothing is counted.
	_, err = runner(t, cfg, withUsage(), day).Run(context.Background(), day)
	require.NoError(t, err)
	markdown, second := readReport(t, root, day)

	_, ok = byPass(second, PassEnrich)
	assert.False(t, ok, "a reused enrichment costs nothing and so is not counted")

	assert.Contains(t, markdown, "THIS RUN",
		"the output has to say the figures are per run, or near-zero tokens reads as a bug someone fixes into a double-count")
}

func TestReportPricesSpendOnlyWhenConfigured(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	client := &stubClient{
		enrich: func(llm.Request) (llm.Response, error) {
			return llm.Response{
				Content: enrichJSON("summary", 0.8),
				Usage:   llm.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000},
			}, nil
		},
	}

	// Without prices the report counts tokens and says nothing about money.
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)
	markdown, unpriced := readReport(t, root, day)
	for _, spend := range unpriced.Spend {
		assert.False(t, spend.Priced)
		assert.Zero(t, spend.Cost)
		assert.Empty(t, spend.Currency)
	}
	assert.NotContains(t, markdown, "ZWL")

	// With prices, every priced line carries the currency verbatim.
	cfg.Aggregator.Prices = config.Prices{
		Currency: "ZWL",
		PerMillionTokens: map[string]config.ModelPrice{
			"small-model": {Input: 2, Output: 8},
			"large-model": {Input: 3, Output: 9},
		},
	}
	// Force a fresh enrichment so there is spend to price.
	require.NoError(t, os.Remove(store.New(root).StatePath(day)))

	_, err = runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)
	markdown, priced := readReport(t, root, day)

	var enrich PassSpend
	for _, spend := range priced.Spend {
		if spend.Pass == PassEnrich {
			enrich = spend
		}
	}
	require.True(t, enrich.Priced)
	assert.InDelta(t, 10.0, enrich.Cost, 0.0001, "1M prompt at 2 plus 1M completion at 8")
	assert.Equal(t, "ZWL", enrich.Currency)
	assert.Contains(t, markdown, "ZWL")
	assert.Empty(t, priced.UnpricedModels)
}

// TestAnUnpricedModelIsNamedRatherThanCountedAsFree is the silent-understatement
// this refuses: a total that quietly omits a model looks like a cheap day.
func TestAnUnpricedModelIsNamedRatherThanCountedAsFree(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	cfg.Aggregator.Prices = config.Prices{
		Currency:         "ZWL",
		PerMillionTokens: map[string]config.ModelPrice{"small-model": {Input: 2, Output: 8}},
	}

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, report := readReport(t, root, day)
	assert.Equal(t, []string{"large-model"}, report.UnpricedModels)
	assert.Contains(t, markdown, "No configured rate for: large-model")

	for _, spend := range report.Spend {
		if spend.Model == "large-model" {
			assert.False(t, spend.Priced)
			assert.Zero(t, spend.Cost, "an unpriced model must not be priced at zero")
		}
	}
}

// TestReportStatesTheLimitItCannotSee keeps ADR-0005 §7's honest limit in the
// output rather than only in the ADR.
func TestReportStatesTheLimitItCannotSee(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, _ := readReport(t, root, day)
	assert.Contains(t, markdown, "cannot explain a topic no configured source covers",
		"four of the five absence reasons are recoverable; the fifth is invisible by construction, "+
			"and the report must not imply its reasons are a complete account")
}

// TestTheReportIsWrittenAfterEveryEdition is the one ordering the design
// imposes, since the report describes what the editions selected.
func TestTheReportIsWrittenAfterEveryEdition(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "\neditions:\n  alpha: {}\n  zulu: {}\n",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)
	require.Len(t, report.Editions, 2, "the report describes every edition, so all of them ran first")

	// Both digests exist on disk by the time the report does.
	for _, id := range []string{"alpha", "zulu"} {
		_, digest := readDigest(t, root, day, id)
		assert.Equal(t, id, digest.Edition)
	}
}

// TestAFailedEditionStillAppearsInTheReport keeps the report honest about an
// edition whose digest was never written.
func TestAFailedEditionStillAppearsInTheReport(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", store.Item{Source: "a-source", Content: "body"})

	cfg.Editions = map[string]config.Edition{
		"broken": {Profile: filepath.Join(t.TempDir(), "absent.md")},
		"works":  {Profile: cfg.RelevanceProfile},
	}

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.Error(t, err)

	markdown, report := readReport(t, root, day)
	require.Len(t, report.Editions, 2)

	byID := map[string]ReportEdition{}
	for _, edition := range report.Editions {
		byID[edition.ID] = edition
	}
	assert.NotEmpty(t, byID["broken"].Failed, "an edition that failed says so rather than reading as empty")
	assert.Empty(t, byID["works"].Failed)
	assert.Contains(t, markdown, "FAILED")
}

// TestReportRecordsWallTime covers the figure a frozen clock hides: with a
// clock that never advances every duration is zero, so nothing would notice the
// measurement being dropped altogether.
func TestReportRecordsWallTime(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body"})

	// The clock advances ONLY while a call is in flight, never on a bare
	// reading. That is what makes this test able to tell "timed the call" from
	// "timed nothing": a clock that ticked on every read would report a
	// plausible duration even if the timer started after the call returned.
	tick := day
	clock := func() time.Time { return tick }

	client := &stubClient{}
	timed := &clockAdvancingClient{inner: client, advance: func() { tick = tick.Add(500 * time.Millisecond) }}

	r, err := NewRunner(cfg,
		WithClient(timed),
		WithClock(clock),
		WithLogger(quietLogger()),
	)
	require.NoError(t, err)
	_, err = r.Run(context.Background(), day)
	require.NoError(t, err)

	markdown, report := readReport(t, root, day)
	require.NotEmpty(t, report.Spend)
	for _, spend := range report.Spend {
		assert.Equal(t, int64(500*spend.Calls), spend.WallMillis,
			"each call takes exactly 500ms of clock, so the total is the call count times that: %s/%s",
			spend.Pass, spend.Model)
	}
	assert.NotContains(t, markdown, ", 0ms")
}

// clockAdvancingClient moves the test clock forward while a call is in flight,
// so a measured duration can only be non-zero if the timer bracketed the call.
type clockAdvancingClient struct {
	inner   Completer
	advance func()
}

func (c *clockAdvancingClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	c.advance()
	return c.inner.Complete(ctx, req)
}

// TestReportPluralisesCounts keeps the operator-facing document reading like
// writing rather than output.
func TestReportPluralisesCounts(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "body a"},
		store.Item{Source: "b-source", URL: "https://example.com/b", Content: "body b"})

	writeCollected(t, root, day, map[string]collect.SourceOutcome{
		"a-source": {Ran: true, Items: 1},
		"b-source": {Ran: true, Items: 4},
	})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)

	markdown, _ := readReport(t, root, day)
	assert.Contains(t, markdown, "1 item\n", "one is singular")
	assert.Contains(t, markdown, "4 items", "more than one is plural")
	assert.Contains(t, markdown, "2 candidates")
	assert.NotContains(t, markdown, "1 items")
	assert.NotContains(t, markdown, "1 calls")
}

// TestEveryEnrichedItemGetsAnAbsenceReason closes the one hole ADR-0005 §7 does
// not allow: "no reason at all" is reserved for items that never entered the
// pipeline, so an item that was enriched and then vanishes from an edition's
// accounting is the outcome this section must never produce.
//
// The case is an item that is BOTH below the salience floor and outside a
// narrowed edition's source list. It is dormant while the shipped floor is 0
// and starts firing the moment a real floor is set — which is the next planned
// change to this code.
func TestEveryEnrichedItemGetsAnAbsenceReason(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "\neditions:\n  narrow: {sources: [a-source]}\n  wide: {}\n",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "substantive"},
		store.Item{Source: "b-source", URL: "https://example.com/b", Content: "junk"})

	client := &stubClient{
		enrich: func(req llm.Request) (llm.Response, error) {
			if strings.Contains(req.Messages[1].Content, "junk") {
				return llm.Response{Content: enrichJSON("navigation chrome", 0.05)}, nil
			}
			return llm.Response{Content: enrichJSON("real substance", 0.9)}, nil
		},
	}
	_, err := runner(t, cfg, client, day, WithSalienceFloor(0.5)).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)

	// Every item that reached enrichment is accounted for by every edition,
	// either as selected or with a reason.
	enrichedNames := map[string]bool{}
	for _, item := range report.Items {
		if item.Status == StatusEnriched {
			enrichedNames[item.Item] = true
		}
	}
	require.Len(t, enrichedNames, 2)

	for _, edition := range report.Editions {
		accounted := map[string]string{}
		for _, name := range edition.Selected {
			accounted[name] = "selected"
		}
		for _, absence := range edition.Absent {
			accounted[absence.Item] = absence.Reason
		}
		for name := range enrichedNames {
			assert.Contains(t, accounted, name,
				"edition %q leaves %s unaccounted for; an item that entered the pipeline must never "+
					"disappear from the report without a reason", edition.ID, name)
		}
	}

	byID := map[string]ReportEdition{}
	for _, edition := range report.Editions {
		byID[edition.ID] = edition
	}

	// The narrow edition: the junk item is below the floor AND out of scope.
	// Both are true; the source list is what gets reported, because it is the
	// answer scoped to this edition.
	narrow := map[string]string{}
	for _, absence := range byID["narrow"].Absent {
		narrow[absence.Source] = absence.Reason
	}
	assert.Equal(t, ReasonNotInSources, narrow["b-source"])

	// The wide edition draws on everything, so there the same item's absence is
	// the floor — which is the reason that actually applies to it there.
	wide := map[string]string{}
	for _, absence := range byID["wide"].Absent {
		wide[absence.Source] = absence.Reason
	}
	assert.Equal(t, ReasonBelowFloor, wide["b-source"])
}

// --- a failed selection is one item's failure, not the edition's (#64) ---

// selectFailsFor builds a select hook that returns truncated JSON for one
// title and a clean selection for every other item. The malformed body is the
// shape actually observed: an unterminated string in the reason field, which is
// what a response-length ceiling produces.
func selectFailsFor(title string) func(req llm.Request) (llm.Response, error) {
	return func(req llm.Request) (llm.Response, error) {
		var text string
		for _, m := range req.Messages {
			text += m.Content
		}
		if strings.Contains(text, title) {
			return llm.Response{
				Content: `{"selected": false, "score": 0.1, "reason": "The profile requests`,
			}, nil
		}
		return llm.Response{Content: selectionJSON(true, 0.9)}, nil
	}
}

func threeItems() []store.Item {
	return []store.Item{
		{Source: "a-source", URL: "https://example.com/alpha", Title: "Alpha", Content: "body"},
		{Source: "a-source", URL: "https://example.com/bravo", Title: "Bravo", Content: "body"},
		{Source: "a-source", URL: "https://example.com/charlie", Title: "Charlie", Content: "body"},
	}
}

// TestOneMalformedSelectDoesNotCostTheEdition is the regression. One item's
// unusable reply used to return from runEdition, discarding every selection
// already made and writing no digest at all.
func TestOneMalformedSelectDoesNotCostTheEdition(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", threeItems()...)

	client := &stubClient{sel: selectFailsFor("Bravo")}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)

	require.NoError(t, err, "one item the model could not judge is not the edition's failure")
	require.Len(t, result.Editions, 1)

	edition := result.Editions[0]
	assert.Equal(t, 2, edition.Selected, "the other two items are still selected")
	assert.Equal(t, 1, edition.Unjudged)
	assert.Equal(t, 1, result.Unjudged())
	assert.False(t, edition.Empty)

	// The digest exists and carries the survivors, which is the whole point.
	_, digest := readDigest(t, root, day, config.DefaultEdition)
	require.Len(t, digest.Items, 2)
	assert.Equal(t, 1, digest.Unjudged)

	titles := []string{digest.Items[0].Title, digest.Items[1].Title}
	assert.ElementsMatch(t, []string{"Alpha", "Charlie"}, titles)
}

// TestAnUnjudgedItemIsNotReportedAsRejected keeps the two states apart. "Not
// selected by this edition's profile" asserts the profile was applied and said
// no; a failed select means it was never asked. Folding one into the other
// would make the report state something untrue about the profile.
func TestAnUnjudgedItemIsNotReportedAsRejected(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", threeItems()...)

	client := &stubClient{sel: selectFailsFor("Bravo")}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)
	require.Len(t, report.Editions, 1)

	// Item filenames are slugged from the source, so the title has to be
	// resolved through the report's own item list rather than guessed.
	var bravo string
	for _, item := range report.Items {
		if item.Title == "Bravo" {
			bravo = item.Item
		}
	}
	require.NotEmpty(t, bravo, "the item that failed selection was still enriched")

	var found ReportAbsence
	for _, absence := range report.Editions[0].Absent {
		if absence.Item == bravo {
			found = absence
		}
		assert.NotEqual(t, ReasonNotSelected, absence.Reason,
			"nothing was rejected by the profile in this run")
	}

	require.NotEmpty(t, found.Item, "the unjudged item still gets an absence, per ADR-0005 §7")
	assert.Equal(t, ReasonSelectFailed, found.Reason)
	assert.Contains(t, found.Error, "usable JSON",
		"the absence carries the cause, so the report answers what the log used to")
}

// TestMalformedSelectIsRetriedOnce covers the insurance half: a truncation is a
// property of one generation, so the item gets a second chance before it is
// recorded as unjudged.
func TestMalformedSelectIsRetriedOnce(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "Alpha", Content: "body"})

	var attempts int
	client := &stubClient{sel: func(_ llm.Request) (llm.Response, error) {
		attempts++
		if attempts == 1 {
			return llm.Response{Content: `{"selected": true, "score": 0.9, "reason": "trunc`}, nil
		}
		return llm.Response{Content: selectionJSON(true, 0.9)}, nil
	}}

	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Equal(t, 2, attempts, "the item is asked about exactly twice")
	require.Len(t, result.Editions, 1)
	assert.Equal(t, 1, result.Editions[0].Selected, "the retry's answer is the one used")
	assert.Zero(t, result.Editions[0].Unjudged)
}

// TestSelectRetryStopsAtOne bounds the insurance. The failure is
// input-dependent, so retrying harder spends most on the items least likely to
// come back clean.
func TestSelectRetryStopsAtOne(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "Alpha", Content: "body"})

	client := &stubClient{sel: selectFailsFor("Alpha")}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Len(t, client.callsIn("select"), 2, "one retry, not a loop")
	assert.Equal(t, 1, result.Editions[0].Unjudged)
}

// TestEveryAttemptIsBilled guards the accounting. Both calls were made and
// charged, so reporting only the successful one would make a retry look free.
func TestEveryAttemptIsBilled(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "Alpha", Content: "body"})

	var attempts int
	client := &stubClient{sel: func(_ llm.Request) (llm.Response, error) {
		attempts++
		if attempts == 1 {
			return llm.Response{
				Content: `{"selected": true, "score": 0.9, "reason": "trunc`,
				Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5},
			}, nil
		}
		return llm.Response{
			Content: selectionJSON(true, 0.9),
			Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5},
		}, nil
	}}

	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)
	var selectSpend PassSpend
	for _, spend := range report.Spend {
		if spend.Pass == PassSelect {
			selectSpend = spend
		}
	}
	assert.Equal(t, 2, selectSpend.Calls, "the discarded attempt was still paid for")
	assert.Equal(t, 20, selectSpend.PromptTokens)
}

// TestEmptyDigestSaysWhenNothingWasAsked is the reader-facing half. An edition
// that judged nothing produces the same empty page as one whose profile matched
// nothing, and the two are opposite facts about whether the engine worked.
func TestEmptyDigestSaysWhenNothingWasAsked(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", threeItems()...)

	// Every select fails, so nothing is selected and nothing was judged.
	client := &stubClient{sel: func(_ llm.Request) (llm.Response, error) {
		return llm.Response{Content: `{"selected": false, "score": 0.1, "reason": "trunc`}, nil
	}}

	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)
	require.Equal(t, 3, result.Unjudged())

	markdown, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.True(t, digest.Empty)
	assert.Equal(t, 3, digest.Unjudged)
	assert.Contains(t, markdown, "could not be assessed",
		"an empty digest must not imply the profile matched nothing when nothing was asked")

	// The delivered markdown never carries the cause, following the rule
	// silentSourcesNote already sets: this is what a sink hands to a reader.
	assert.NotContains(t, markdown, "usable JSON")
}

// --- the digest's language (#60) ---

// germanTitles are the shape the reported defect had: headlines copied verbatim
// from a publisher writing in one language, under summaries generated in
// another.
func germanTitles() []store.Item {
	return []store.Item{
		{Source: "a-source", URL: "https://example.com/eins", Title: "Bahnstreik endet nach vier Tagen", Content: "body"},
		{Source: "a-source", URL: "https://example.com/zwei", Title: "Hafen meldet Rekordumschlag", Content: "body"},
	}
}

// titlesJSON builds a title-pass reply from item index to headline.
func titlesJSON(t *testing.T, translated map[int]string) string {
	t.Helper()
	type entry struct {
		Index int    `json:"index"`
		Title string `json:"title"`
	}
	// Sorted, so a reply is the same string every run and a failure is
	// reproducible rather than order-dependent.
	indexes := make([]int, 0, len(translated))
	for i := range translated {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)

	entries := make([]entry, 0, len(indexes))
	for _, i := range indexes {
		entries = append(entries, entry{Index: i, Title: translated[i]})
	}
	raw, err := json.Marshal(map[string][]entry{"titles": entries})
	require.NoError(t, err)
	return string(raw)
}

// numberedTitlesIn reads back the numbered list a title request carried, as
// index to headline.
//
// Tests translate by CONTENT rather than by position because the order of an
// edition's selected items is the store's, not the fixture's — an assumption
// about which item is index 0 silently attaches one headline to another item
// and then asserts about the wrong one.
func numberedTitlesIn(t *testing.T, req llm.Request) map[int]string {
	t.Helper()
	out := map[int]string{}
	for _, line := range strings.Split(req.Messages[len(req.Messages)-1].Content, "\n") {
		number, title, ok := strings.Cut(line, ". ")
		if !ok {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSpace(number))
		if err != nil {
			continue
		}
		out[index] = title
	}
	require.NotEmpty(t, out, "a title request always carries at least one numbered headline")
	return out
}

// translateWith answers a title request the way the pass asks to be answered:
// headlines present in the dictionary come back rendered, everything else is
// omitted as already being in the reader's language.
func translateWith(t *testing.T, dictionary map[string]string) func(llm.Request) (llm.Response, error) {
	return func(req llm.Request) (llm.Response, error) {
		reply := map[int]string{}
		for index, title := range numberedTitlesIn(t, req) {
			if rendered, ok := dictionary[title]; ok {
				reply[index] = rendered
			}
		}
		return llm.Response{Content: titlesJSON(t, reply)}, nil
	}
}

// germanToEnglish is the fixture dictionary, so every test that translates
// agrees about what a rendered headline looks like.
var germanToEnglish = map[string]string{
	"Bahnstreik endet nach vier Tagen": "Rail strike ends after four days",
	"Hafen meldet Rekordumschlag":      "Port reports record throughput",
}

const englishEdition = `
language: English
`

// TestNoLanguageMeansNoTitlePassAtAll pins the default. An engine upgraded to
// this build with an untouched config must behave exactly as it did before:
// same passes, same prompts, same spend.
func TestNoLanguageMeansNoTitlePassAtAll(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "", germanTitles()...)

	client := &stubClient{}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Empty(t, client.callsIn("title"), "no language is configured, so there is nothing to render into one")

	require.Len(t, client.callsIn("digest"), 1)
	assert.Equal(t, digestSystemPrompt, client.callsIn("digest")[0].Messages[0].Content,
		"an edition with no language sends the writing instructions it sent before this pass existed")

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	require.Len(t, digest.Items, 2)
	for _, item := range digest.Items {
		assert.Empty(t, item.TitleTranslated)
	}
}

// TestATranslatedTitleIsCarriedAlongsideTheOriginal is the fix. The original is
// what matches the linked page, so it is never replaced.
func TestATranslatedTitleIsCarriedAlongsideTheOriginal(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", englishEdition, germanTitles()...)

	client := &stubClient{title: translateWith(t, germanToEnglish)}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	require.Len(t, digest.Items, 2)

	byOriginal := map[string]DigestItem{}
	for _, item := range digest.Items {
		byOriginal[item.Title] = item
	}

	first, ok := byOriginal["Bahnstreik endet nach vier Tagen"]
	require.True(t, ok, "the publisher's headline is still the one under `title`")
	assert.Equal(t, "Rail strike ends after four days", first.TitleTranslated)

	second, ok := byOriginal["Hafen meldet Rekordumschlag"]
	require.True(t, ok)
	assert.Equal(t, "Port reports record throughput", second.TitleTranslated)

	assert.Empty(t, digest.TitlesFailed)
}

// TestAHeadlineAlreadyInTheReaderLanguageCostsNoCallOfItsOwn is the common case:
// the pass is told to omit those, so they consume no output tokens and produce
// nothing to apply. The absent field is what a consumer falls back on.
func TestAHeadlineAlreadyInTheReaderLanguageCostsNoCallOfItsOwn(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", englishEdition,
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "Rail strike ends", Content: "body"},
		store.Item{Source: "a-source", URL: "https://example.com/b", Title: "Port reports record", Content: "body"})

	client := &stubClient{}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Len(t, client.callsIn("title"), 1,
		"one call for the edition, whatever it decides about the individual headlines")

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	require.Len(t, digest.Items, 2)
	for _, item := range digest.Items {
		assert.Empty(t, item.TitleTranslated, "nothing needed rendering, so nothing is carried")
		assert.NotEmpty(t, item.Title)
	}
}

// TestTheTitlePassRunsOnceForTheWholeEdition pins the unit. A per-item call
// would multiply the cost of naming a language by the size of the digest.
func TestTheTitlePassRunsOnceForTheWholeEdition(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", englishEdition, threeItems()...)

	client := &stubClient{}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	calls := client.callsIn("title")
	require.Len(t, calls, 1, "three selected items, one call")

	var sent string
	for _, m := range calls[0].Messages {
		sent += m.Content
	}
	for _, title := range []string{"Alpha", "Bravo", "Charlie"} {
		assert.Contains(t, sent, title, "every selected headline is in the one request")
	}
}

// TestTheTitlePassOnlyPaysForSelectedHeadlines keeps the pass downstream of
// selection. The items an edition passed over are the majority and none of them
// reaches a reader, so none is worth a token.
func TestTheTitlePassOnlyPaysForSelectedHeadlines(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", englishEdition, threeItems()...)

	client := &stubClient{sel: func(req llm.Request) (llm.Response, error) {
		var text string
		for _, m := range req.Messages {
			text += m.Content
		}
		return llm.Response{Content: selectionJSON(strings.Contains(text, "Alpha"), 0.9)}, nil
	}}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	calls := client.callsIn("title")
	require.Len(t, calls, 1)

	var sent string
	for _, m := range calls[0].Messages {
		sent += m.Content
	}
	assert.Contains(t, sent, "Alpha")
	assert.NotContains(t, sent, "Bravo", "an item this edition passed over never reaches the reader")
	assert.NotContains(t, sent, "Charlie")
}

// TestAFailedTitlePassCostsTheLanguageNotTheDigest is the posture the select
// pass settled on in #64, applied to the pass added here: the digest is written,
// every headline keeps the language it was published in, and the run says so.
func TestAFailedTitlePassCostsTheLanguageNotTheDigest(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", englishEdition, germanTitles()...)

	client := &stubClient{title: func(llm.Request) (llm.Response, error) {
		return llm.Response{Content: `{"titles": [{"index": 0, "title": "Rail strike`}, nil
	}}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err, "a language is not worth a day's digest")

	require.Len(t, result.Editions, 1)
	assert.True(t, result.Editions[0].TitlesFailed)

	markdown, digest := readDigest(t, root, day, config.DefaultEdition)
	require.Len(t, digest.Items, 2, "the digest is still written and still carries both items")
	for _, item := range digest.Items {
		assert.Empty(t, item.TitleTranslated)
		assert.NotEmpty(t, item.Title, "a headline in the wrong language beats no headline")
	}

	assert.NotEmpty(t, digest.TitlesFailed,
		"a failed run and a day where every publisher already wrote in English render identically without this")
	assert.Contains(t, markdown, "shown as they were published")

	// The delivered markdown carries the outcome and never the cause, following
	// unjudgedNote and silentSourcesNote (ADR-0005 §8).
	assert.NotContains(t, markdown, "usable JSON")
}

// TestAMisalignedTitleReplyIsRejectedWholesale is the sharp one. A reply that
// answers about a headline it was not given is not a good reply with one bad
// entry: its other entries may be shifted by one, and a shifted headline is
// wrong in the way that looks right. Applying the entries that happen to land on
// real items is the failure this rejects.
func TestAMisalignedTitleReplyIsRejectedWholesale(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", englishEdition, germanTitles()...)

	client := &stubClient{title: func(req llm.Request) (llm.Response, error) {
		// One real entry alongside one index that was never sent. Keeping the
		// valid entry is the point of the test: a parser that skipped the bad
		// entry would apply this one.
		reply := map[int]string{99: "A headline about nothing we sent"}
		for index, title := range numberedTitlesIn(t, req) {
			if rendered, ok := germanToEnglish[title]; ok {
				reply[index] = rendered
			}
		}
		return llm.Response{Content: titlesJSON(t, reply)}, nil
	}}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	require.Len(t, result.Editions, 1)
	assert.True(t, result.Editions[0].TitlesFailed)

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	for _, item := range digest.Items {
		assert.Empty(t, item.TitleTranslated,
			"none of a misaligned reply is applied, including the entries that land on real items")
	}
}

// TestMalformedTitleReplyIsRetriedOnce mirrors the select pass: one retry on a
// reply that did not parse.
func TestMalformedTitleReplyIsRetriedOnce(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", englishEdition, germanTitles()...)

	var calls int
	translate := translateWith(t, map[string]string{
		"Bahnstreik endet nach vier Tagen": "Rail strike ends after four days",
	})
	client := &stubClient{title: func(req llm.Request) (llm.Response, error) {
		calls++
		if calls == 1 {
			return llm.Response{Content: "not json at all"}, nil
		}
		return translate(req)
	}}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Equal(t, 2, calls, "asked once more, and once only")

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	var translated int
	for _, item := range digest.Items {
		if item.TitleTranslated != "" {
			translated++
		}
	}
	assert.Equal(t, 1, translated, "the second reply is applied")
	assert.Empty(t, digest.TitlesFailed)
}

// TestEveryTitleAttemptIsBilled keeps a retry from reading as free, which is the
// same trap the select pass's usage accounting had.
func TestEveryTitleAttemptIsBilled(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", englishEdition, germanTitles()...)

	client := &stubClient{title: func(llm.Request) (llm.Response, error) {
		return llm.Response{
			Content: "not json at all",
			Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5},
		}, nil
	}}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)
	var title *PassSpend
	for i := range report.Spend {
		if report.Spend[i].Pass == PassTitle {
			title = &report.Spend[i]
		}
	}
	require.NotNil(t, title, "a pass that ran is a pass that is accounted for")
	assert.Equal(t, 2, title.Calls)
	assert.Equal(t, 20, title.PromptTokens, "both attempts were paid for, so both are counted")
}

// TestATitleThatCameBackUnchangedIsNotCarriedTwice guards the payload against
// an item whose original and translated headline are the same string, which
// would have a consumer render a headline alongside itself.
func TestATitleThatCameBackUnchangedIsNotCarriedTwice(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", englishEdition,
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "Rail strike ends", Content: "body"})

	client := &stubClient{title: translateWith(t, map[string]string{
		"Rail strike ends": "Rail strike ends",
	})}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	require.Len(t, digest.Items, 1)
	assert.Empty(t, digest.Items[0].TitleTranslated,
		"a headline that came back identical needed nothing, whatever the pass thought it was doing")
}

// titleCountsFrom runs one day with the given title behaviour and returns what
// the report recorded about the pass.
func titleCountsFrom(t *testing.T, editionYAML string, title func(llm.Request) (llm.Response, error)) (ReportEdition, EditionResult) {
	t.Helper()
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", editionYAML, germanTitles()...)

	result, err := runner(t, cfg, &stubClient{title: title}, day).Run(context.Background(), day)
	require.NoError(t, err)

	_, report := readReport(t, root, day)
	require.Len(t, report.Editions, 1)
	require.Len(t, result.Editions, 1)
	return report.Editions[0], result.Editions[0]
}

// TestTheReportSaysWhatTheTitlePassDid is the record issue #5 asks for: what the
// pass was given and what came back, rather than only that it was billed.
func TestTheReportSaysWhatTheTitlePassDid(t *testing.T) {
	edition, _ := titleCountsFrom(t, englishEdition, translateWith(t, map[string]string{
		"Bahnstreik endet nach vier Tagen": "Rail strike ends after four days",
	}))

	titles := edition.Titles
	require.NotNil(t, titles, "the pass ran, so the report carries what it did")
	assert.Equal(t, 2, titles.Offered, "both selected headlines were sent")
	assert.Equal(t, 1, titles.Returned, "the reply named one of them")
	assert.Equal(t, 1, titles.Changed, "and that one differed from the original")
}

// TestAPassThatNamedNothingIsNotAPassThatLeftEverythingAlone is the sharp one,
// and the whole point of issue #5.
//
// 🚨 Both cases change no headline and both cost the same handful of completion
// tokens, so the digest, the markdown and the spend row are identical across
// them. One is an edition whose sources already publish in its language — the
// correct quiet outcome — and the other is a pass that declined to engage with
// the headlines at all. Only Returned separates them, which is why a bare
// changed-count would not have closed this.
func TestAPassThatNamedNothingIsNotAPassThatLeftEverythingAlone(t *testing.T) {
	declinedEdition, _ := titleCountsFrom(t, englishEdition, func(llm.Request) (llm.Response, error) {
		return llm.Response{Content: `{"titles": []}`}, nil
	})
	leftAloneEdition, _ := titleCountsFrom(t, englishEdition, translateWith(t, map[string]string{
		"Bahnstreik endet nach vier Tagen": "Bahnstreik endet nach vier Tagen",
		"Hafen meldet Rekordumschlag":      "Hafen meldet Rekordumschlag",
	}))

	declined, leftAlone := declinedEdition.Titles, leftAloneEdition.Titles
	require.NotNil(t, declined)
	require.NotNil(t, leftAlone)

	assert.Empty(t, declinedEdition.TitlesFailed, "neither case is a failure")
	assert.Empty(t, leftAloneEdition.TitlesFailed, "neither case is a failure")

	assert.Equal(t, 0, declined.Changed, "neither case changes a headline")
	assert.Equal(t, 0, leftAlone.Changed, "neither case changes a headline")

	assert.Equal(t, 0, declined.Returned, "the reply named no headline at all")
	assert.Equal(t, 2, leftAlone.Returned,
		"the reply named both and they needed nothing, which the record must not flatten into the case above")
}

// TestAnUnattemptedTitlePassLeavesNoRecord keeps the absence meaningful: the
// record is present exactly when the pass was attempted, so a reader can take
// its absence as "never ran" rather than as "ran and did nothing".
func TestAnUnattemptedTitlePassLeavesNoRecord(t *testing.T) {
	edition, _ := titleCountsFrom(t, "", nil)
	assert.Nil(t, edition.Titles, "no language is configured, so there was nothing to attempt")
}

// TestAFailedTitlePassStillSaysWhatItWasAsked pairs the counts with the cause.
// The pass was attempted, so the record exists; it changed nothing, and the
// edition's failure carries why.
func TestAFailedTitlePassStillSaysWhatItWasAsked(t *testing.T) {
	reported, edition := titleCountsFrom(t, englishEdition, func(llm.Request) (llm.Response, error) {
		return llm.Response{Content: `{"titles": [{"index": 0, "title": "Rail strike`}, nil
	})

	titles := reported.Titles
	require.NotNil(t, titles, "an attempted pass is recorded even when it could not be completed")
	assert.Equal(t, 2, titles.Offered)
	assert.Equal(t, 0, titles.Returned)
	assert.Equal(t, 0, titles.Changed)
	assert.True(t, edition.TitlesFailed, "the counts say what was asked; this says why nothing came back")

	// 🚨 The counts here are identical to a reply that named no headline, so the
	// report carries the cause beside them. Without it the owner's own file
	// renders a failure and a correct no-op as one record, which is the shape
	// this pass was changed to stop doing.
	assert.NotEmpty(t, reported.TitlesFailed,
		"the report says why, rather than sending its owner to the digest to find out")
}

// TestATitlePassThatCouldNotBeCalledIsStillRecorded is the other failure mode.
// An unusable reply and a call that never completed both leave the headlines as
// published, and both were attempted — so both carry the record, and the reader
// is never left inferring "attempted" from the absence of one.
func TestATitlePassThatCouldNotBeCalledIsStillRecorded(t *testing.T) {
	reported, edition := titleCountsFrom(t, englishEdition, func(llm.Request) (llm.Response, error) {
		return llm.Response{}, errors.New("the aggregator could not be reached")
	})

	titles := reported.Titles
	require.NotNil(t, titles, "the pass was attempted; only the call failed")
	assert.Equal(t, 2, titles.Offered)
	assert.Equal(t, 0, titles.Returned)
	assert.Equal(t, 0, titles.Changed)
	assert.True(t, edition.TitlesFailed)
	assert.NotEmpty(t, reported.TitlesFailed, "and the report carries the cause, not only the digest")
}

// TestTheWritingPassIsToldTheLanguageAndKeepsTheOriginal pins the second half of
// the fix: the digest's prose is written in the edition's language rather than
// left emergent, and the writer is given both headlines.
func TestTheWritingPassIsToldTheLanguageAndKeepsTheOriginal(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", englishEdition, germanTitles()...)

	client := &stubClient{title: translateWith(t, map[string]string{
		"Bahnstreik endet nach vier Tagen": "Rail strike ends after four days",
	})}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	calls := client.callsIn("digest")
	require.Len(t, calls, 1)

	system := calls[0].Messages[0].Content
	assert.Contains(t, system, "WRITE IN English")
	assert.Contains(t, system, "Never replace the original")

	user := calls[0].Messages[len(calls[0].Messages)-1].Content
	assert.Contains(t, user, "## Rail strike ends after four days", "the entry leads with the reader's language")
	assert.Contains(t, user, "Original headline: Bahnstreik endet nach vier Tagen")
	assert.NotContains(t, user, "Original headline: Hafen meldet Rekordumschlag",
		"a headline already in the reader's language has no second version to keep")
}

// TestAnEditionUsesItsOwnLanguage keeps language per edition, which is the whole
// reason it is config and not a line in a shared profile.
func TestAnEditionUsesItsOwnLanguage(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "profile", `
language: English
editions:
  personal: {}
  auswaertiges: {language: Deutsch}
`, germanTitles()...)

	client := &stubClient{}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	languages := map[string]bool{}
	for _, call := range client.callsIn("title") {
		languages[call.Messages[len(call.Messages)-1].Content] = true
	}
	require.Len(t, client.callsIn("title"), 2, "one call per edition")

	var sawEnglish, sawGerman bool
	for body := range languages {
		if strings.Contains(body, "English") {
			sawEnglish = true
		}
		if strings.Contains(body, "Deutsch") {
			sawGerman = true
		}
	}
	assert.True(t, sawEnglish, "the edition inheriting the top-level language asks for it")
	assert.True(t, sawGerman, "the edition naming its own overrides it")
}

func TestParseTitles(t *testing.T) {
	asked := map[int]bool{0: true, 1: true}

	t.Run("applies what it was asked about", func(t *testing.T) {
		got, err := parseTitles(`{"titles": [{"index": 1, "title": "A headline"}]}`, asked)
		require.NoError(t, err)
		assert.Equal(t, map[int]string{1: "A headline"}, got)
	})

	t.Run("an empty list means nothing needed changing", func(t *testing.T) {
		got, err := parseTitles(`{"titles": []}`, asked)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("an empty headline is the same statement as omitting it", func(t *testing.T) {
		got, err := parseTitles(`{"titles": [{"index": 0, "title": "  "}]}`, asked)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("an index it was not given fails the whole reply", func(t *testing.T) {
		_, err := parseTitles(`{"titles": [{"index": 0, "title": "ok"}, {"index": 7, "title": "?"}]}`, asked)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not aligned")
	})

	t.Run("a repeated index fails the whole reply", func(t *testing.T) {
		_, err := parseTitles(`{"titles": [{"index": 0, "title": "one"}, {"index": 0, "title": "two"}]}`, asked)
		require.Error(t, err)
	})

	t.Run("unusable JSON is an error", func(t *testing.T) {
		_, err := parseTitles(`{"titles": [`, asked)
		require.Error(t, err)
	})
}
