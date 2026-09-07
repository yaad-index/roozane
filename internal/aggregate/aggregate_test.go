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

// stubClient answers each call from a queue, recording what it was asked. A
// queue rather than a single canned answer, because the two passes differ and a
// test that cannot tell them apart proves very little.
type stubClient struct {
	replies []llm.Response
	errs    []error
	calls   []llm.Request
}

func (s *stubClient) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	s.calls = append(s.calls, req)
	i := len(s.calls) - 1
	if i < len(s.errs) && s.errs[i] != nil {
		return llm.Response{}, s.errs[i]
	}
	if i < len(s.replies) {
		return s.replies[i], nil
	}
	return llm.Response{Content: "unexpected extra call"}, nil
}

func judgementJSON(relevant bool, score float64, points []string, reason string) string {
	raw, _ := json.Marshal(Judgement{Relevant: relevant, Score: score, Points: points, Reason: reason})
	return string(raw)
}

// fixture builds a data root with a profile and the given items already
// collected, plus a loaded config pointing at both.
func fixture(t *testing.T, day time.Time, profile string, items ...store.Item) (*config.Config, string) {
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
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o600))

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	return cfg, root
}

func runner(t *testing.T, cfg *config.Config, client Completer, now time.Time) *Runner {
	t.Helper()
	r, err := NewRunner(cfg,
		WithClient(client),
		WithClock(func() time.Time { return now }),
		WithLogger(quietLogger()),
	)
	require.NoError(t, err)
	return r
}

func TestRunWritesBothDigestFiles(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "I care about container orchestration.",
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "Orchestration news", Content: "the raw text"},
	)

	client := &stubClient{replies: []llm.Response{
		{Content: judgementJSON(true, 0.9, []string{"A released version 2."}, "matches orchestration"), Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}},
		{Content: "- A released version 2.", Usage: llm.Usage{PromptTokens: 50, CompletionTokens: 8, TotalTokens: 58}},
	}}

	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Items)
	assert.Equal(t, 1, result.Relevant)
	assert.False(t, result.Empty)
	assert.Equal(t, 150, result.Usage.PromptTokens, "both passes are accounted for")

	mdPath, jsonPath := store.New(root).DigestPaths(day, config.DefaultEdition)

	md, err := os.ReadFile(mdPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(md), "# Digest — 2026-09-04")
	assert.Contains(t, string(md), "A released version 2.")

	var digest Digest
	raw, err := os.ReadFile(jsonPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &digest))

	assert.Equal(t, DigestSchema, digest.Schema, "sinks depend on this shape, so it is versioned from day one")
	assert.Equal(t, "2026-09-04", digest.Day)
	assert.False(t, digest.Empty)
	require.Len(t, digest.Items, 1)
	assert.Equal(t, "a-source", digest.Items[0].Source)
	assert.Equal(t, "https://example.com/a", digest.Items[0].URL)
	assert.Equal(t, []string{"A released version 2."}, digest.Items[0].Points)
}

// TestQuietDayWritesAnExplicitEmptyDigest covers the product law directly: a day
// where nothing clears the bar is a correct outcome with real output, not a
// missing file and not filler.
func TestQuietDayWritesAnExplicitEmptyDigest(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "I care about container orchestration.",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "an article about gardening"},
	)

	client := &stubClient{replies: []llm.Response{
		{Content: judgementJSON(false, 0.1, nil, "not about orchestration")},
	}}

	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.True(t, result.Empty)
	assert.Equal(t, 1, result.Suppressed)

	// The digest pass is skipped entirely: there is nothing to write from, and
	// asking a model to write about nothing is how filler gets produced.
	assert.Len(t, client.calls, 1, "a quiet day must not spend a digest call")

	mdPath, jsonPath := store.New(root).DigestPaths(day, config.DefaultEdition)

	md, err := os.ReadFile(mdPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(md), emptyDigestMarker)

	var digest Digest
	raw, err := os.ReadFile(jsonPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &digest))
	assert.True(t, digest.Empty, "a quiet day and a run that never happened must stay distinguishable")
	assert.Empty(t, digest.Items)
}

func TestTwoPassesUseTheirConfiguredModels(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "I care about orchestration.",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "text"},
	)

	client := &stubClient{replies: []llm.Response{
		{Content: judgementJSON(true, 0.8, []string{"a point"}, "matches")},
		{Content: "- a point"},
	}}

	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	require.Len(t, client.calls, 2)
	// Cost scales with the size of the task: a small model per item, a larger
	// one for the day's digest.
	assert.Equal(t, "small-model", client.calls[0].Model)
	assert.Equal(t, "large-model", client.calls[1].Model)

	// The reader's profile reaches both passes verbatim.
	for i, call := range client.calls {
		require.Len(t, call.Messages, 2)
		assert.Contains(t, call.Messages[1].Content, "I care about orchestration.", "call %d carries the profile", i)
	}
	// The item pass sees the raw text; the digest pass sees extracted points.
	assert.Contains(t, client.calls[0].Messages[1].Content, "text")
	assert.Contains(t, client.calls[1].Messages[1].Content, "a point")
}

func TestReRunReusesRecordedJudgements(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "I care about orchestration.",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "text"},
	)

	first := &stubClient{replies: []llm.Response{
		{Content: judgementJSON(true, 0.8, []string{"a point"}, "matches"), Usage: llm.Usage{PromptTokens: 100}},
		{Content: "- a point"},
	}}
	_, err := runner(t, cfg, first, day).Run(context.Background(), day)
	require.NoError(t, err)
	require.Len(t, first.calls, 2)

	second := &stubClient{replies: []llm.Response{{Content: "- a point"}}}
	result, err := runner(t, cfg, second, day).Run(context.Background(), day)
	require.NoError(t, err)

	// The item pass is not paid for twice; only the digest is rebuilt.
	assert.Equal(t, 1, result.Reused)
	require.Len(t, second.calls, 1)
	assert.Equal(t, "large-model", second.calls[0].Model)
	assert.Zero(t, result.Usage.PromptTokens, "a reused judgement costs nothing and must not be counted as spend")
}

func TestOneFailingItemDoesNotCostTheDigest(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "I care about orchestration.",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "first"},
		store.Item{Source: "a-source", URL: "https://example.com/b", Content: "second"},
	)

	client := &stubClient{
		errs:    []error{errors.New("upstream refused"), nil, nil},
		replies: []llm.Response{{}, {Content: judgementJSON(true, 0.9, []string{"a point"}, "matches")}, {Content: "- a point"}},
	}

	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Failed)
	assert.Equal(t, 1, result.Relevant)
	assert.False(t, result.Empty, "the surviving item still produces a digest")

	// The failure is inspectable per item, per ADR-0002 §5.
	var state State
	raw, err := os.ReadFile(store.New(root).StatePath(day)) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &state))

	var failed int
	for _, item := range state.Items {
		if item.Status == StatusFailed {
			failed++
			assert.Contains(t, item.Error, "upstream refused")
		}
	}
	assert.Equal(t, 1, failed)
}

func TestFailedItemsAreRetriedOnTheNextRun(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "I care about orchestration.",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "first"},
	)

	failing := &stubClient{errs: []error{errors.New("upstream refused")}, replies: []llm.Response{{}}}
	_, err := runner(t, cfg, failing, day).Run(context.Background(), day)
	require.NoError(t, err)

	retry := &stubClient{replies: []llm.Response{
		{Content: judgementJSON(true, 0.9, []string{"a point"}, "matches")},
		{Content: "- a point"},
	}}
	result, err := runner(t, cfg, retry, day).Run(context.Background(), day)
	require.NoError(t, err)

	// A failure is not a verdict, so it must not be reused as one.
	assert.Zero(t, result.Reused)
	assert.Equal(t, 1, result.Relevant)
	assert.Len(t, retry.calls, 2)
}

func TestEmptyProfileIsRejected(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, _ := fixture(t, day, "   \n",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "text"},
	)

	client := &stubClient{}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)

	// An empty profile matches nothing, so every day would be silently empty.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is empty")
	assert.Empty(t, client.calls, "nothing should be spent judging against an empty profile")
}

func TestNoItemsStillWritesAnEmptyDigest(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "I care about orchestration.")

	client := &stubClient{}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Zero(t, result.Items)
	assert.True(t, result.Empty)
	assert.Empty(t, client.calls)

	mdPath, _ := store.New(root).DigestPaths(day, config.DefaultEdition)
	assert.FileExists(t, mdPath, "a day that collected nothing is still a day the engine ran")
}

func TestParseJudgement(t *testing.T) {
	t.Run("plain json", func(t *testing.T) {
		j, err := parseJudgement(`{"relevant":true,"score":0.7,"points":["p"],"reason":"r"}`)
		require.NoError(t, err)
		assert.True(t, j.Relevant)
		assert.InDelta(t, 0.7, j.Score, 0.0001)
	})

	t.Run("fenced json", func(t *testing.T) {
		// Models wrap JSON in a fence despite being asked not to; discarding a
		// correct answer over its packaging would be the wrong trade.
		j, err := parseJudgement("```json\n{\"relevant\":false,\"score\":0.1,\"points\":[],\"reason\":\"no\"}\n```")
		require.NoError(t, err)
		assert.False(t, j.Relevant)
	})

	t.Run("prose instead of json", func(t *testing.T) {
		_, err := parseJudgement("I think this item is quite relevant, actually.")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "usable JSON")
	})

	t.Run("relevant with no points is a contradiction", func(t *testing.T) {
		_, err := parseJudgement(`{"relevant":true,"score":0.9,"points":[],"reason":"r"}`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no data points")
	})
}

func TestDigestFilesAreWrittenAtomically(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "I care about orchestration.",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "text"},
	)

	client := &stubClient{replies: []llm.Response{
		{Content: judgementJSON(true, 0.9, []string{"a point"}, "matches")},
		{Content: "- a point"},
	}}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	entries, err := os.ReadDir(store.New(root).EditionDir(config.DefaultEdition))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".tmp-"),
			"a leftover temp file would be read as a mystery digest: %s", e.Name())
	}
}

// TestSuppressionIsTheDefaultInThePrompt guards the wording that carries the
// product law. A prompt that merely asks "is this relevant?" produces a bar
// that drifts downward, so the instruction has to survive edits.
func TestSuppressionIsTheDefaultInThePrompt(t *testing.T) {
	assert.Contains(t, itemSystemPrompt, "SUPPRESSION IS THE DEFAULT")
	assert.Contains(t, itemSystemPrompt, "When in doubt, it is not relevant")
	assert.Contains(t, digestSystemPrompt, "LENGTH FOLLOWS SIGNAL")

	// No provider may be named anywhere in what the engine sends.
	for _, prompt := range []string{itemSystemPrompt, digestSystemPrompt} {
		lower := strings.ToLower(prompt)
		for _, banned := range []string{"openai", "anthropic", "gpt-", "claude", "gemini", "mistral", "llama"} {
			assert.NotContains(t, lower, banned, "prompts must stay provider-agnostic")
		}
	}
}

// TestAFailureIsNeverReusedAsAVerdict pins the guard directly. The normal
// failure path records no judgement, so the nil check alone happens to cover
// it — but a state file carrying BOTH a failed status and a judgement (an older
// writer, a hand edit) must still be re-judged, because a failure is not a
// verdict. Without this the status check is untested and reads as removable.
func TestAFailureIsNeverReusedAsAVerdict(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "I care about orchestration.",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "text"},
	)

	items, err := store.New(root).ReadItems(day)
	require.NoError(t, err)
	require.Len(t, items, 1)

	// Seed state that is failed AND carries a judgement.
	seeded := State{
		Schema: stateSchema,
		Day:    store.Day(day),
		Items: map[string]ItemState{
			items[0].Filename: {
				Status:    StatusFailed,
				Error:     "upstream refused",
				Judgement: &Judgement{Relevant: true, Score: 0.9, Points: []string{"stale point"}, Reason: "stale"},
			},
		},
	}
	raw, err := json.MarshalIndent(seeded, "", "  ")
	require.NoError(t, err)
	require.NoError(t, store.New(root).WriteAtomic(store.New(root).StatePath(day), raw))

	client := &stubClient{replies: []llm.Response{
		{Content: judgementJSON(true, 0.5, []string{"fresh point"}, "re-judged")},
		{Content: "- fresh point"},
	}}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Zero(t, result.Reused, "a failed entry must be re-judged, not reused")
	require.Len(t, client.calls, 2)

	_, jsonPath := store.New(root).DigestPaths(day, config.DefaultEdition)
	digestRaw, err := os.ReadFile(jsonPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(digestRaw), "fresh point")
	assert.NotContains(t, string(digestRaw), "stale point")
}

func TestStateFromADifferentSchemaIsNotReused(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "I care about orchestration.",
		store.Item{Source: "a-source", URL: "https://example.com/a", Content: "text"},
	)

	items, err := store.New(root).ReadItems(day)
	require.NoError(t, err)

	// State written by some other version, carrying a judgement whose fields may
	// not mean what this version thinks they mean.
	foreign := map[string]any{
		"schema": stateSchema + 1,
		"day":    store.Day(day),
		"items": map[string]any{
			items[0].Filename: map[string]any{
				"status":    StatusRelevant,
				"judgement": map[string]any{"relevant": true, "score": 0.9, "points": []string{"stale"}, "reason": "stale"},
			},
		},
	}
	raw, err := json.MarshalIndent(foreign, "", "  ")
	require.NoError(t, err)
	require.NoError(t, store.New(root).WriteAtomic(store.New(root).StatePath(day), raw))

	client := &stubClient{replies: []llm.Response{
		{Content: judgementJSON(true, 0.5, []string{"fresh"}, "re-judged")},
		{Content: "- fresh"},
	}}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	// Relabelling another version's bookkeeping as current is worse than paying
	// for one re-run.
	assert.Zero(t, result.Reused)
	require.Len(t, client.calls, 2)

	_, jsonPath := store.New(root).DigestPaths(day, config.DefaultEdition)
	digest, err := os.ReadFile(jsonPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(digest), "fresh")
	assert.NotContains(t, string(digest), "stale")
}
