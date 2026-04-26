// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestExtractTagsAgainstFakeServer simulates an Ollama server that returns a
// well-formed JSON object inside the chat message. It locks down the contract
// we expect from the daemon (POST /api/chat returns {message:{content:"..."}}).
func TestExtractTagsAgainstFakeServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": `{"tags":["go","rss","ollama"]}`,
			},
			"done": true,
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-model", 5*time.Second)
	tags, err := c.ExtractTags(context.Background(), "Title", "https://x", "content")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tags) != 3 || tags[0] != "go" || tags[2] != "ollama" {
		t.Fatalf("unexpected tags: %v", tags)
	}
}

func TestScoreEntryAgainstFakeServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": `{"score": 0.83}`,
			},
			"done": true,
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-model", 5*time.Second)
	profile := []ProfileSample{{Title: "past article", Tags: []string{"go"}, Starred: true}}
	score, err := c.ScoreEntry(context.Background(), profile, "Title", "https://x", []string{"go"}, "excerpt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score < 0.82 || score > 0.84 {
		t.Fatalf("unexpected score: %v", score)
	}
}

func TestScoreEntryEmptyProfileShortCircuits(t *testing.T) {
	// No HTTP server: if the client called out, this would fail with a
	// connection error. With an empty profile the function must return 0.5
	// without contacting the daemon.
	c := NewClient("http://127.0.0.1:1", "test-model", 100*time.Millisecond)
	score, err := c.ScoreEntry(context.Background(), nil, "T", "U", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score != 0.5 {
		t.Fatalf("expected neutral 0.5 score, got %v", score)
	}
}

func TestChatRetriesTransientServerError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{"role": "assistant", "content": `{"tags":["ok"]}`},
			"done":    true,
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "m", 5*time.Second)
	tags, err := c.ExtractTags(context.Background(), "T", "U", "C")
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if len(tags) != 1 || tags[0] != "ok" {
		t.Fatalf("unexpected tags after retry: %v", tags)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 calls (1 fail + 1 retry), got %d", got)
	}
}

func TestChatDoesNotRetryClientError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad model", http.StatusBadRequest)
	}))
	defer server.Close()

	c := NewClient(server.URL, "m", 5*time.Second)
	_, err := c.ExtractTags(context.Background(), "T", "U", "C")
	if err == nil {
		t.Fatal("expected error on 4xx, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected no retry on 4xx, got %d calls", got)
	}
}

func TestChatGivesUpAfterSecondTransientFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "still down", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c := NewClient(server.URL, "m", 5*time.Second)
	_, err := c.ExtractTags(context.Background(), "T", "U", "C")
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", got)
	}
}

func TestChatRespectsCanceledContextDuringBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := NewClient(server.URL, "m", 5*time.Second)

	// Cancel right after the first failure: the backoff sleep should bail out
	// instead of waiting the full 500ms before retrying.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := c.ExtractTags(ctx, "T", "U", "C")
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	if elapsed := time.Since(start); elapsed >= retryBackoff {
		t.Fatalf("expected early exit on cancel, took %v", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestScoreEntryPromptBuildsProfileAndExcerpt captures the request body the
// client sends to Ollama and checks the user message contains the profile
// markers and a truncated excerpt — these are the parts the model needs to
// produce a calibrated score.
func TestScoreEntryPromptBuildsProfileAndExcerpt(t *testing.T) {
	var captured chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{"role": "assistant", "content": `{"score":0.4}`},
			"done":    true,
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "m", 5*time.Second)
	profile := []ProfileSample{
		{Title: "Distributed systems primer", Tags: []string{"distributed", "systems"}, Starred: true},
		{Title: "A boring CRUD post", Tags: []string{"web"}, Starred: false},
	}
	// Use a marker char that does not appear anywhere else in the prompt so
	// the truncation assertion below is unambiguous.
	long := strings.Repeat("Z", 5000)
	if _, err := c.ScoreEntry(context.Background(), profile, "Cand", "https://example.test", []string{"go"}, long); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if captured.Format != "json" {
		t.Errorf("expected format=json, got %q", captured.Format)
	}
	if len(captured.Messages) != 2 || captured.Messages[0].Role != "system" {
		t.Fatalf("unexpected messages: %+v", captured.Messages)
	}
	user := captured.Messages[1].Content
	for _, want := range []string{
		"User profile",
		"[starred] Distributed systems primer",
		"[read] A boring CRUD post",
		"Title: Cand",
		"Tags: go",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q\n--- prompt ---\n%s", want, user)
		}
	}
	// Excerpt is truncated to 1500 chars in score.go; the original was 5000.
	if got := strings.Count(user, "Z"); got != 1500 {
		t.Errorf("expected excerpt truncated to 1500 chars, got %d", got)
	}
}

func TestExtractTagsPromptTruncatesContent(t *testing.T) {
	var captured chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{"role": "assistant", "content": `{"tags":["a"]}`},
			"done":    true,
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "m", 5*time.Second)
	long := strings.Repeat("Z", 10000)
	if _, err := c.ExtractTags(context.Background(), "T", "https://example.test", long); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	user := captured.Messages[1].Content
	if got := strings.Count(user, "Z"); got != 6000 {
		t.Errorf("expected content truncated to 6000 chars, got %d", got)
	}
}

func TestExtractTagsHandlesCodeFenceFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": "```json\n{\"tags\":[\"a\",\"b\"]}\n```",
			},
			"done": true,
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "m", 5*time.Second)
	tags, err := c.ExtractTags(context.Background(), "T", "U", "C")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("unexpected tags: %v", tags)
	}
}
