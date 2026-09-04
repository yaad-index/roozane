// Package llm is a minimal client for the standard chat-completions HTTP
// shape.
//
// It is deliberately provider-agnostic (ADR-0001 §3): the engine knows an
// endpoint, a model name and a credential read from the environment, and
// nothing about who is on the other end. No provider is named, assumed or
// endorsed anywhere in this package, and none should be added — pointing the
// base URL somewhere else must remain a configuration change.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Role values for a chat message.
const (
	RoleSystem = "system"
	RoleUser   = "user"
)

// Message is one turn of a chat-completions request.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is one completion call.
type Request struct {
	Model    string
	Messages []Message

	// Temperature is sent only when set. A nil value leaves the choice to the
	// endpoint rather than imposing one the operator did not ask for.
	Temperature *float64
}

// Usage is the token accounting a response reports, recorded per item so a
// re-run can be explained in terms of what it cost.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is the assistant's reply plus its accounting.
type Response struct {
	Content string
	Usage   Usage
}

// Client talks to one chat-completions endpoint.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New builds a client for a base URL, with the credential the caller has
// already resolved from the environment. The credential is never read from or
// written to configuration here.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

// wireRequest is the on-the-wire request body.
type wireRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
}

// wireResponse is the subset of the response shape the engine uses. Unknown
// fields are ignored, so an endpoint returning extra data is not an error.
type wireResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// maxErrorBody bounds how much of a failing response is quoted back. Enough to
// diagnose, not enough to dump a page of HTML into a log.
const maxErrorBody = 2048

// Complete performs one chat completion.
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	if req.Model == "" {
		return Response{}, errors.New("no model configured")
	}
	if len(req.Messages) == 0 {
		return Response{}, errors.New("no messages to send")
	}

	// A direct conversion: wireRequest exists to own the JSON tags, not to hold
	// different data. Should the two ever need to differ, this stops compiling,
	// which is the right moment to split them apart again.
	body, err := json.Marshal(wireRequest(req))
	if err != nil {
		return Response{}, fmt.Errorf("encode request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// The credential is in a header, not the URL, so the wrapped error
		// cannot carry it — but keep the message about the endpoint, not the
		// request, so nothing from the body leaks either.
		return Response{}, fmt.Errorf("call %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body; a close error says nothing actionable

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("chat completion failed with %s: %s", resp.Status, snippet(raw))
	}

	var parsed wireResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("decode response: %w (body: %s)", err, snippet(raw))
	}
	// Some endpoints report a failure in the body with a 200 status.
	if parsed.Error != nil && parsed.Error.Message != "" {
		return Response{}, fmt.Errorf("chat completion returned an error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, errors.New("chat completion returned no choices")
	}

	return Response{
		Content: parsed.Choices[0].Message.Content,
		Usage:   parsed.Usage,
	}, nil
}

// snippet bounds quoted response text and collapses newlines so a failure fits
// one log line.
func snippet(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > maxErrorBody {
		text = text[:maxErrorBody] + "…"
	}
	return strings.Join(strings.Fields(text), " ")
}
