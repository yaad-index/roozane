package deliver

import (
	"context"
	"encoding/json"
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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/store"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func day(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(store.DayFormat, value)
	require.NoError(t, err)
	return parsed
}

// fixture writes a digest for a day and loads a config carrying the given
// sinks block.
func fixture(t *testing.T, d time.Time, empty bool, sinksYAML string) (*config.Config, string) {
	t.Helper()

	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	s := store.New(root)

	markdown := "# Digest — " + store.Day(d) + "\n\n- A point worth reading.\n"
	items := `[{"source":"a-source","url":"https://example.com/a","score":0.9,"reason":"r","points":["A point worth reading."]}]`
	if empty {
		markdown = "# Digest — " + store.Day(d) + "\n\n_Nothing today cleared the relevance bar._\n"
		items = "[]"
	}
	structured := `{"schema":1,"day":"` + store.Day(d) + `","generated_at":"2026-09-04T07:00:00Z","empty":` +
		map[bool]string{true: "true", false: "false"}[empty] + `,"items":` + items + `}`

	mdPath, jsonPath := s.DigestPaths(d, config.DefaultEdition)
	require.NoError(t, s.WriteAtomic(mdPath, []byte(markdown)))
	require.NoError(t, s.WriteAtomic(jsonPath, []byte(structured)))

	body := "data_root: " + root + `
relevance_profile: profile.md
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small, digest: large}
sources:
  a-source: {collector: feed, cadence: daily}
` + sinksYAML

	cfgPath := filepath.Join(dir, "roozane.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o600))

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	return cfg, dir
}

// recordingSink captures what it was handed.
type recordingSink struct {
	got  []Digest
	fail error
}

func (r *recordingSink) Deliver(_ context.Context, d Digest) error {
	r.got = append(r.got, d)
	return r.fail
}

func TestRunDeliversToEverySink(t *testing.T) {
	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false, "sinks:\n  one: {type: file}\n  two: {type: file}\n")

	sinks := map[string]*recordingSink{"one": {}, "two": {}}
	result, err := NewRunner(cfg,
		WithLogger(quietLogger()),
		WithSinkBuilder(func(id string, _ config.Sink) (Sink, error) { return sinks[id], nil }),
	).Run(context.Background(), d)
	require.NoError(t, err)

	assert.False(t, result.Failed())
	require.Len(t, result.Sinks, 2)
	// Sorted ids, so a run reports the same way twice.
	assert.Equal(t, "one", result.Sinks[0].ID)
	assert.Equal(t, "two", result.Sinks[1].ID)

	for id, sink := range sinks {
		require.Len(t, sink.got, 1, "sink %s", id)
		assert.Contains(t, sink.got[0].Markdown, "A point worth reading.")
		assert.False(t, sink.got[0].Empty)
		assert.Equal(t, "2026-09-04", sink.got[0].Day)
	}
}

func TestOneFailingSinkDoesNotStopTheOthers(t *testing.T) {
	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false, "sinks:\n  broken: {type: file}\n  working: {type: file}\n")

	working := &recordingSink{}
	result, err := NewRunner(cfg,
		WithLogger(quietLogger()),
		WithSinkBuilder(func(id string, _ config.Sink) (Sink, error) {
			if id == "broken" {
				return &recordingSink{fail: errors.New("chat is down")}, nil
			}
			return working, nil
		}),
	).Run(context.Background(), d)
	require.NoError(t, err)

	// A chat delivery being down is no reason for the file copy not to happen.
	assert.True(t, result.Failed())
	assert.Len(t, working.got, 1)
}

// TestAnEmptyDigestIsStillDelivered is the product law at the delivery layer:
// suppressing a quiet day would make it indistinguishable from a broken
// pipeline for the reader.
func TestAnEmptyDigestIsStillDelivered(t *testing.T) {
	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, true, "sinks:\n  one: {type: file}\n")

	sink := &recordingSink{}
	result, err := NewRunner(cfg,
		WithLogger(quietLogger()),
		WithSinkBuilder(func(string, config.Sink) (Sink, error) { return sink, nil }),
	).Run(context.Background(), d)
	require.NoError(t, err)

	assert.True(t, result.Empty)
	require.Len(t, sink.got, 1)
	assert.True(t, sink.got[0].Empty)
	assert.Contains(t, sink.got[0].Markdown, "Nothing today cleared the relevance bar")
}

func TestMissingDigestIsNotTreatedAsEmpty(t *testing.T) {
	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false, "sinks:\n  one: {type: file}\n")

	// Ask for a day that was never aggregated.
	sink := &recordingSink{}
	_, err := NewRunner(cfg,
		WithLogger(quietLogger()),
		WithSinkBuilder(func(string, config.Sink) (Sink, error) { return sink, nil }),
	).Run(context.Background(), day(t, "2026-09-05"))

	// Absent is not empty: delivering an invented empty digest would erase the
	// difference ADR-0002 goes out of its way to preserve.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has not run")
	assert.Empty(t, sink.got)
}

func TestNoSinksIsNotAFailure(t *testing.T) {
	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false, "")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)

	// The digest is on disk; a reader who only wants files has a complete
	// pipeline without configuring a sink.
	assert.Empty(t, result.Sinks)
	assert.False(t, result.Failed())
}

func TestUnknownSinkTypeIsReportedNotIgnored(t *testing.T) {
	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false, "sinks:\n  odd: {type: carrier-pigeon}\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)

	require.Len(t, result.Sinks, 1)
	require.Error(t, result.Sinks[0].Err)
	assert.Contains(t, result.Sinks[0].Err.Error(), "carrier-pigeon")
	assert.Contains(t, result.Sinks[0].Err.Error(), builtinSinkList)
}

// --- file sink ---

func TestFileSink(t *testing.T) {
	d := day(t, "2026-09-04")
	out := filepath.Join(t.TempDir(), "nested", "digest-{day}.md")
	cfg, _ := fixture(t, d, false, "sinks:\n  local: {type: file, params: {path: \""+out+"\"}}\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.NoError(t, result.Sinks[0].Err)

	written := strings.ReplaceAll(out, dayPlaceholder, "2026-09-04")
	raw, err := os.ReadFile(written) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(raw), "A point worth reading.")

	// The placeholder is what lets a config keep every day rather than
	// overwrite one file.
	assert.NotContains(t, written, dayPlaceholder)

	entries, err := os.ReadDir(filepath.Dir(written))
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".tmp-"), "a leftover temp file: %s", e.Name())
	}
}

func TestFileSinkJSONFormatPassesTheBytesThrough(t *testing.T) {
	d := day(t, "2026-09-04")
	out := filepath.Join(t.TempDir(), "digest.json")
	cfg, root := fixture(t, d, false, "sinks:\n  local: {type: file, params: {path: \""+out+"\", format: json}}\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.NoError(t, result.Sinks[0].Err)

	_, jsonPath := store.New(filepath.Join(root, "data")).DigestPaths(d, config.DefaultEdition)
	onDisk, err := os.ReadFile(jsonPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	delivered, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	require.NoError(t, err)

	// Byte-identical: the JSON on disk is the contract sinks were promised, so
	// it is copied rather than re-encoded.
	assert.Equal(t, onDisk, delivered)
}

func TestFileSinkRejectsBadConfig(t *testing.T) {
	d := day(t, "2026-09-04")

	for name, tc := range map[string]struct{ sinks, want string }{
		"no path":    {sinks: "sinks:\n  local: {type: file}\n", want: "params.path"},
		"bad format": {sinks: "sinks:\n  local: {type: file, params: {path: /tmp/x, format: pdf}}\n", want: "must be markdown or json"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := fixture(t, d, false, tc.sinks)
			result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
			require.NoError(t, err)
			require.Len(t, result.Sinks, 1)
			require.Error(t, result.Sinks[0].Err)
			assert.Contains(t, result.Sinks[0].Err.Error(), tc.want)
		})
	}
}

// --- chat sink ---

func TestTelegramSinkSendsTheDigest(t *testing.T) {
	t.Setenv("TEST_BOT_TOKEN", "bot-token-value")

	var gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false,
		"sinks:\n  chat: {type: telegram, params: {chat_id: \"12345\", token_env: TEST_BOT_TOKEN, api_base: \""+srv.URL+"\"}}\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.NoError(t, result.Sinks[0].Err)

	assert.Equal(t, "/botbot-token-value/sendMessage", gotPath)
	assert.Equal(t, "12345", gotBody["chat_id"])
	assert.Contains(t, gotBody["text"], "A point worth reading.")
}

func TestTelegramSinkNeedsItsCredentialInTheEnvironment(t *testing.T) {
	d := day(t, "2026-09-04")
	cfg, _ := fixture(t, d, false,
		"sinks:\n  chat: {type: telegram, params: {chat_id: \"1\", token_env: DEFINITELY_UNSET_TOKEN}}\n")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.Error(t, result.Sinks[0].Err)
	assert.Contains(t, result.Sinks[0].Err.Error(), "DEFINITELY_UNSET_TOKEN")
}

// TestTelegramErrorsNeverCarryTheToken matters because this API puts the
// credential in the URL path, which net/http errors quote in full.
func TestTelegramErrorsNeverCarryTheToken(t *testing.T) {
	const token = "super-secret-bot-token"
	t.Setenv("TEST_BOT_TOKEN", token)

	t.Run("api error echoing the path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			// An API that echoes the request path back would publish the token.
			_, _ = io.WriteString(w, `{"ok":false,"description":"unauthorized for /bot`+token+`/sendMessage"}`)
		}))
		defer srv.Close()

		d := day(t, "2026-09-04")
		cfg, _ := fixture(t, d, false,
			"sinks:\n  chat: {type: telegram, params: {chat_id: \"1\", token_env: TEST_BOT_TOKEN, api_base: \""+srv.URL+"\"}}\n")

		result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
		require.NoError(t, err)
		require.Error(t, result.Sinks[0].Err)
		assert.NotContains(t, result.Sinks[0].Err.Error(), token)
		assert.Contains(t, result.Sinks[0].Err.Error(), "«redacted»")
	})

	t.Run("transport failure", func(t *testing.T) {
		d := day(t, "2026-09-04")
		cfg, _ := fixture(t, d, false,
			"sinks:\n  chat: {type: telegram, params: {chat_id: \"1\", token_env: TEST_BOT_TOKEN, api_base: \"http://127.0.0.1:1\"}}\n")

		result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
		require.NoError(t, err)
		require.Error(t, result.Sinks[0].Err)
		// net/http wraps the full URL, and the token is a path segment of it.
		assert.NotContains(t, result.Sinks[0].Err.Error(), token)
	})
}

func TestSplitMessage(t *testing.T) {
	assert.Equal(t, []string{"short"}, splitMessage("short", 100))

	// Splits at a line boundary so a break does not land mid-sentence.
	chunks := splitMessage("aaaa\nbbbb\ncccc", 10)
	require.Len(t, chunks, 2)
	assert.Equal(t, "aaaa\nbbbb", chunks[0])
	assert.Equal(t, "cccc", chunks[1])

	// A single over-long line still has to go somewhere.
	long := strings.Repeat("x", 25)
	chunks = splitMessage(long, 10)
	require.Len(t, chunks, 3)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), 10)
	}
	assert.Equal(t, long, strings.Join(chunks, ""),
		"splitting must not lose text: dropping the tail would be the delivery layer editing the digest")
}

func TestTelegramSinkSplitsALongDigest(t *testing.T) {
	t.Setenv("TEST_BOT_TOKEN", "t")

	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		sent = append(sent, body["text"])
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	d := day(t, "2026-09-04")
	cfg, root := fixture(t, d, false,
		"sinks:\n  chat: {type: telegram, params: {chat_id: \"1\", token_env: TEST_BOT_TOKEN, api_base: \""+srv.URL+"\"}}\n")

	// Overwrite the digest with one past the platform's message ceiling.
	long := "# Digest\n\n" + strings.Repeat("A line of the digest that the reader wants.\n", 200)
	mdPath, _ := store.New(filepath.Join(root, "data")).DigestPaths(d, config.DefaultEdition)
	require.NoError(t, store.New(filepath.Join(root, "data")).WriteAtomic(mdPath, []byte(long)))
	require.Greater(t, len(long), telegramLimit, "the fixture must exceed the ceiling to exercise splitting")

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.NoError(t, result.Sinks[0].Err)

	assert.Greater(t, len(sent), 1, "a digest past the ceiling has to be split, not rejected")
	for _, msg := range sent {
		assert.LessOrEqual(t, len(msg), telegramLimit)
	}
}

// TestFileSinkNeverExposesATornFile is the third instance of the same lesson in
// this codebase: asserting "no leftover .tmp files" does NOT distinguish an
// atomic write from an in-place one, because a plain os.WriteFile leaves no
// temp file either. The property that matters is what a concurrent reader sees,
// and this sink writes to an operator-chosen path that a static site build or a
// sync tool may be reading.
func TestFileSinkNeverExposesATornFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "digest.md")

	bodyA := "A" + strings.Repeat("a", 512*1024)
	bodyB := "B" + strings.Repeat("b", 512*1024)

	sink := &fileSink{path: out, format: "markdown"}
	require.NoError(t, sink.Deliver(context.Background(), Digest{Day: "2026-09-04", Markdown: bodyA}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			body := bodyA
			if i%2 == 1 {
				body = bodyB
			}
			if err := sink.Deliver(context.Background(), Digest{Day: "2026-09-04", Markdown: body}); err != nil {
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

		raw, err := os.ReadFile(out) //nolint:gosec // test-controlled path
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		reads++
		require.True(t, string(raw) == bodyA || string(raw) == bodyB,
			"read a partial digest: torn write (len %d)", len(raw))
	}
}

// TestSplitMessageNeverBisectsACharacter is the regression test for silent
// corruption: the fallback cut for an unbroken line used a raw byte index, so a
// multi-byte character straddling the limit was split in half and each half
// encoded as U+FFFD. The API accepts both messages happily and the reader sees
// two replacement characters where one real character was — the delivery layer
// editing the digest, which splitMessage exists not to do.
//
// Persian is the case that matters first here, and its characters are two bytes
// each, so a boundary landing mid-character is not exotic.
func TestSplitMessageNeverBisectsACharacter(t *testing.T) {
	for name, text := range map[string]string{
		"persian":          strings.Repeat("روزنه", 800),
		"mixed with ascii": strings.Repeat("روزنه digest ", 300),
		"four-byte runes":  strings.Repeat("🌍", 2000),
		"three-byte runes": strings.Repeat("日本語", 1000),
	} {
		t.Run(name, func(t *testing.T) {
			require.Greater(t, len(text), telegramLimit, "the fixture must exceed the limit to exercise the split")
			require.NotContains(t, text, "\n", "this must exercise the no-newline fallback, not the line-boundary path")

			chunks := splitMessage(text, telegramLimit)
			require.Greater(t, len(chunks), 1)

			for i, chunk := range chunks {
				assert.True(t, utf8.ValidString(chunk), "chunk %d is not valid UTF-8: a character was bisected", i)
				assert.NotContains(t, chunk, "�", "chunk %d carries a replacement character", i)
				assert.LessOrEqual(t, len(chunk), telegramLimit, "chunk %d is over the limit", i)
			}

			// Nothing added, nothing lost: the split is a transport concern and
			// must not change a byte of the digest.
			assert.Equal(t, text, strings.Join(chunks, ""))
		})
	}
}

// TestTelegramSinkDeliversMultiByteTextIntact walks the same case through the
// real send path, since the corruption only became visible after json.Marshal.
func TestTelegramSinkDeliversMultiByteTextIntact(t *testing.T) {
	t.Setenv("TEST_BOT_TOKEN", "t")

	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		sent = append(sent, body["text"])
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	d := day(t, "2026-09-04")
	cfg, root := fixture(t, d, false,
		"sinks:\n  chat: {type: telegram, params: {chat_id: \"1\", token_env: TEST_BOT_TOKEN, api_base: \""+srv.URL+"\"}}\n")

	digest := strings.Repeat("روزنه", 1200)
	mdPath, _ := store.New(filepath.Join(root, "data")).DigestPaths(d, config.DefaultEdition)
	require.NoError(t, store.New(filepath.Join(root, "data")).WriteAtomic(mdPath, []byte(digest)))

	result, err := NewRunner(cfg, WithLogger(quietLogger())).Run(context.Background(), d)
	require.NoError(t, err)
	require.NoError(t, result.Sinks[0].Err)

	require.Greater(t, len(sent), 1, "the fixture must have been split to exercise the boundary")
	for i, msg := range sent {
		assert.NotContains(t, msg, "�", "message %d reached the API with a replacement character", i)
	}
	// What the reader receives, concatenated, is exactly what was written.
	assert.Equal(t, digest, strings.Join(sent, ""))
}

// TestSplitMessageTerminatesOnAPathologicalLimit pins the zero-cut guard. The
// rune-boundary walk can reach index 0 only when the limit is smaller than a
// single character — absurd in practice, but without the guard the function
// appends an empty chunk and never advances, so a misconfiguration would hang
// the delivery rather than fail it. The guard's job is termination, and that is
// what this asserts.
func TestSplitMessageTerminatesOnAPathologicalLimit(t *testing.T) {
	const text = "🌍🌍🌍"

	done := make(chan []string, 1)
	go func() { done <- splitMessage(text, 1) }()

	select {
	case chunks := <-done:
		assert.NotEmpty(t, chunks)
		// Whatever it does with an impossible limit, it must not lose input.
		assert.Equal(t, text, strings.Join(chunks, ""))
	case <-time.After(5 * time.Second):
		t.Fatal("splitMessage did not terminate: the zero-cut guard is missing")
	}
}
