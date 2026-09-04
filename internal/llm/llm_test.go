package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKey = "test-credential-value"

func TestCompleteSendsTheStandardShape(t *testing.T) {
	var gotPath, gotAuth, gotType string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotType = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"the reply"}}],
		  "usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)
	}))
	defer srv.Close()

	resp, err := New(srv.URL, testKey, 5*time.Second).Complete(context.Background(), Request{
		Model:    "a-model",
		Messages: []Message{{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "usr"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "/chat/completions", gotPath)
	assert.Equal(t, "Bearer "+testKey, gotAuth)
	assert.Equal(t, "application/json", gotType)
	assert.Equal(t, "a-model", gotBody["model"])
	assert.Len(t, gotBody["messages"], 2)
	// Absent unless the operator asks for one, rather than imposing a default.
	assert.NotContains(t, gotBody, "temperature")

	assert.Equal(t, "the reply", resp.Content)
	assert.Equal(t, 11, resp.Usage.PromptTokens)
	assert.Equal(t, 14, resp.Usage.TotalTokens)
}

func TestCompleteSendsTemperatureOnlyWhenSet(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
	}))
	defer srv.Close()

	temp := 0.2
	_, err := New(srv.URL, testKey, 5*time.Second).Complete(context.Background(), Request{
		Model: "a-model", Messages: []Message{{Role: RoleUser, Content: "u"}}, Temperature: &temp,
	})
	require.NoError(t, err)
	assert.InDelta(t, 0.2, gotBody["temperature"], 0.0001)
}

func TestBaseURLTrailingSlashIsHarmless(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
	}))
	defer srv.Close()

	// An operator writing the base URL with a trailing slash is not a
	// misconfiguration worth a double slash in the request path.
	_, err := New(srv.URL+"/", testKey, 5*time.Second).Complete(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "/chat/completions", gotPath)
}

func TestCompleteErrors(t *testing.T) {
	t.Run("no model", func(t *testing.T) {
		_, err := New("https://example.com", testKey, time.Second).Complete(context.Background(), Request{
			Messages: []Message{{Role: RoleUser, Content: "u"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no model")
	})

	t.Run("no messages", func(t *testing.T) {
		_, err := New("https://example.com", testKey, time.Second).Complete(context.Background(), Request{Model: "m"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no messages")
	})

	t.Run("non-2xx quotes the body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
		}))
		defer srv.Close()

		_, err := New(srv.URL, testKey, 5*time.Second).Complete(context.Background(), Request{
			Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "429")
		assert.Contains(t, err.Error(), "slow down", "the body is what says why")
	})

	t.Run("error reported inside a 200", func(t *testing.T) {
		// Some endpoints answer 200 with a failure in the body; treating that as
		// success would hand the parser an empty completion.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"error":{"message":"context length exceeded","type":"invalid_request"}}`)
		}))
		defer srv.Close()

		_, err := New(srv.URL, testKey, 5*time.Second).Complete(context.Background(), Request{
			Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context length exceeded")
	})

	t.Run("no choices", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"choices":[]}`)
		}))
		defer srv.Close()

		_, err := New(srv.URL, testKey, 5*time.Second).Complete(context.Background(), Request{
			Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no choices")
	})

	t.Run("body is not json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<html>gateway error</html>")
		}))
		defer srv.Close()

		_, err := New(srv.URL, testKey, 5*time.Second).Complete(context.Background(), Request{
			Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "gateway error")
	})
}

// TestErrorsNeverCarryTheCredential is the one that matters for a public repo:
// an error string ends up in logs, in a bus message, and in a PR body.
func TestErrorsNeverCarryTheCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key"}}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, testKey, 5*time.Second).Complete(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey,
		"the credential travels in a header and must never reach an error string")

	// Also true when the transport itself fails, where the URL is quoted.
	_, err = New("http://127.0.0.1:1", testKey, 200*time.Millisecond).Complete(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey)
}

func TestErrorBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("z", maxErrorBody*4))
	}))
	defer srv.Close()

	_, err := New(srv.URL, testKey, 5*time.Second).Complete(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
	})
	require.Error(t, err)
	// Enough to diagnose, not enough to dump a page into a log line.
	assert.Less(t, len(err.Error()), maxErrorBody*2)
}

func TestContextCancellationIsHonoured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"late"}}]}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := New(srv.URL, testKey, 10*time.Second).Complete(ctx, Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
	})
	require.Error(t, err)
}

// TestLargeSuccessfulResponseIsNotTruncated is the regression test for a real
// bug: the read was bounded by maxErrorBody — the limit meant for quoting a
// FAILING body into an error string — so any successful completion over ~2 KiB
// was truncated mid-JSON and died in the decoder.
//
// Every stub in this file was a few dozen bytes, which is exactly why the
// original suite was green: the logic was tested and the size never was.
func TestLargeSuccessfulResponseIsNotTruncated(t *testing.T) {
	// A digest is prose; several kilobytes is ordinary, not exotic.
	longContent := strings.Repeat("A sentence of digest prose that a reader would actually want. ", 400)
	require.Greater(t, len(longContent), maxErrorBody*8, "the fixture must clear the old bound to exercise the bug")

	body, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": longContent}}},
		"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 900, "total_tokens": 905},
	})
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	resp, err := New(srv.URL, testKey, 10*time.Second).Complete(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
	})
	require.NoError(t, err, "a large but well-formed completion must decode")
	assert.Equal(t, longContent, resp.Content, "the content must arrive whole, not clipped")
	assert.Equal(t, 905, resp.Usage.TotalTokens)
}

func TestOversizeResponseIsReportedBySize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Stream past the read ceiling without allocating it all here.
		chunk := strings.Repeat("z", 1<<20)
		for range 9 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	_, err := New(srv.URL, testKey, 30*time.Second).Complete(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
	})
	require.Error(t, err)
	// The size is named as the cause; a truncated body would otherwise fail
	// with a JSON syntax error that explains nothing.
	assert.Contains(t, err.Error(), "larger than the")
	assert.NotContains(t, err.Error(), "decode response")
}

// TestErrorBodyStaysBoundedWithTheLargerReadCeiling guards the other half: the
// read ceiling went up, but what reaches an error string must not.
func TestErrorBodyStaysBoundedWithTheLargerReadCeiling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("z", 200_000))
	}))
	defer srv.Close()

	_, err := New(srv.URL, testKey, 10*time.Second).Complete(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}},
	})
	require.Error(t, err)
	assert.Less(t, len(err.Error()), maxErrorBody*2, "a 200KB failure body must not become a 200KB log line")
}
