// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package ollama implements a thin client around the Ollama HTTP API used by
// the entry enrichment worker (tag extraction and per-user scoring).
package ollama // import "miniflux.app/v2/internal/integration/ollama"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// retryBackoff is the delay before the single retry attempt. Kept short on
// purpose: enrichment is best-effort and we do not want to stall the worker.
const retryBackoff = 500 * time.Millisecond

// transientError marks an Ollama failure that is worth retrying once
// (network blips, 5xx, request setup hiccups). 4xx replies are not wrapped
// because they typically indicate a permanent problem (bad model name,
// malformed payload) that retrying will not fix.
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// Client talks to a local Ollama daemon. It is safe for concurrent use.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

// NewClient builds a client against the given Ollama base URL (e.g.
// http://localhost:11434) and model identifier.
func NewClient(baseURL, model string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{Timeout: timeout},
	}
}

// chatRequest mirrors POST /api/chat. We use it (rather than /api/generate)
// because chat-tuned models follow JSON-output instructions more reliably.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Format   string        `json:"format,omitempty"`
	Options  chatOptions   `json:"options,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumCtx      int     `json:"num_ctx,omitempty"`
}

type chatResponse struct {
	Message chatMessage `json:"message"`
	Done    bool        `json:"done"`
}

// chat issues a non-streaming chat completion and returns the assistant text.
// jsonMode forces Ollama to emit valid JSON when supported by the runtime.
// Transient failures (network errors, 5xx) are retried once with a short
// backoff; permanent ones (4xx, malformed responses) propagate immediately.
func (c *Client) chat(ctx context.Context, system, user string, jsonMode bool) (string, error) {
	const maxAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := c.doChat(ctx, system, user, jsonMode)
		if err == nil {
			return out, nil
		}
		lastErr = err

		var transient *transientError
		if !errors.As(err, &transient) || attempt == maxAttempts {
			return "", err
		}
		slog.Debug("ollama: transient chat error, retrying",
			slog.Int("attempt", attempt),
			slog.Any("error", err))

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(retryBackoff):
		}
	}
	return "", lastErr
}

func (c *Client) doChat(ctx context.Context, system, user string, jsonMode bool) (string, error) {
	payload := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Stream: false,
		Options: chatOptions{
			Temperature: 0.2,
		},
	}
	if jsonMode {
		payload.Format = "json"
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("ollama: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", &transientError{err: fmt.Errorf("ollama: call /api/chat: %w", err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", &transientError{err: fmt.Errorf("ollama: read response: %w", err)}
	}

	if resp.StatusCode != http.StatusOK {
		statusErr := fmt.Errorf("ollama: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		if resp.StatusCode >= 500 {
			return "", &transientError{err: statusErr}
		}
		return "", statusErr
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("ollama: decode response: %w", err)
	}
	return parsed.Message.Content, nil
}
