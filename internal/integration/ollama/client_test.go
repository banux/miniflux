// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
