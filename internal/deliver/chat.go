package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/yaad-index/roozane/internal/config"
)

// telegramParams is what a `type: telegram` sink configures.
type telegramParams struct {
	// ChatID is where the message goes.
	ChatID string `yaml:"chat_id"`

	// TokenEnv names the environment variable holding the bot token — a
	// reference, never the token, for the same reason the aggregator's
	// credential is a reference.
	TokenEnv string `yaml:"token_env"`

	// APIBase allows pointing at a different host. Present so this is testable
	// against a local server without a network, and so a self-hosted relay is a
	// config change rather than a code change.
	APIBase string `yaml:"api_base"`
}

// defaultTelegramAPI is the public bot API root.
const defaultTelegramAPI = "https://api.telegram.org"

// telegramLimit is the platform's per-message character ceiling. A digest can
// exceed it, so messages are split rather than rejected or silently clipped.
const telegramLimit = 4096

type telegramSink struct {
	chatID  string
	token   string
	apiBase string
	client  *http.Client
}

func newTelegramSink(sink config.Sink) (Sink, error) {
	var params telegramParams
	if err := sink.DecodeParams(&params); err != nil {
		return nil, fmt.Errorf("telegram sink params: %w", err)
	}
	if params.ChatID == "" {
		return nil, errors.New("telegram sink needs params.chat_id")
	}
	if params.TokenEnv == "" {
		return nil, errors.New("telegram sink needs params.token_env naming the variable that holds the bot token")
	}

	token := os.Getenv(params.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("environment variable %s is unset or empty: it must hold the bot token", params.TokenEnv)
	}

	apiBase := params.APIBase
	if apiBase == "" {
		apiBase = defaultTelegramAPI
	}

	return &telegramSink{
		chatID:  params.ChatID,
		token:   token,
		apiBase: strings.TrimRight(apiBase, "/"),
		client:  &http.Client{},
	}, nil
}

// Deliver sends the digest as one or more chat messages.
func (s *telegramSink) Deliver(ctx context.Context, digest Digest) error {
	for i, chunk := range splitMessage(digest.Markdown, telegramLimit) {
		if err := s.send(ctx, chunk); err != nil {
			return fmt.Errorf("send message %d of the digest: %w", i+1, err)
		}
	}
	return nil
}

func (s *telegramSink) send(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]string{"chat_id": s.chatID, "text": text})
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}

	// The token is a path segment of this API, which is exactly why no error
	// below may quote the URL: doing so would put the credential in a log.
	url := fmt.Sprintf("%s/bot%s/sendMessage", s.apiBase, s.token)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return errors.New("could not build the request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		// Deliberately not %w on the transport error: net/http errors carry the
		// full URL, and the token is in it.
		return errors.New("the chat API could not be reached")
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body; a close error says nothing actionable

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return errors.New("could not read the chat API response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("chat API returned %s: %s", resp.Status, redactToken(string(raw), s.token))
	}
	return nil
}

// redactToken removes the bot token from anything about to be surfaced. An API
// that echoes the request path back in an error would otherwise publish it.
func redactToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "«redacted»")
}

// splitMessage breaks text into chunks no longer than limit, preferring a line
// boundary so a split does not land mid-sentence.
//
// Splitting rather than truncating is the point: a digest is the product, and
// silently dropping its tail because a transport has a size limit would be the
// delivery layer deciding what the reader sees.
func splitMessage(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}

	var chunks []string
	for len(text) > limit {
		cut := strings.LastIndexByte(text[:limit], '\n')
		if cut <= 0 {
			// No line boundary to use — a single very long line. Cut at the
			// limit rather than growing the chunk past what the API accepts.
			cut = limit
		}
		chunks = append(chunks, strings.TrimRight(text[:cut], "\n"))
		text = strings.TrimLeft(text[cut:], "\n")
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}
