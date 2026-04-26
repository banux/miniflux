// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ollama // import "miniflux.app/v2/internal/integration/ollama"

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const tagSystemPrompt = `You are a content classifier for an RSS reader.
Given an article, return between 3 and 7 short topical tags that describe what the article is about.
Tags must be lowercase, in English, single words or short hyphenated phrases (no spaces), and free of punctuation.
Reply with a JSON object of the form {"tags": ["tag-one", "tag-two"]} and nothing else.`

type tagResponse struct {
	Tags []string `json:"tags"`
}

// ExtractTags asks the model to derive topical tags from the article. The
// content should be the cleaned article body (HTML stripped is fine, the model
// is robust enough). Returns an empty slice rather than an error if the model
// produces no parseable output.
func (c *Client) ExtractTags(ctx context.Context, title, url, content string) ([]string, error) {
	user := fmt.Sprintf("Title: %s\nURL: %s\n\n%s", title, url, truncate(content, 6000))

	raw, err := c.chat(ctx, tagSystemPrompt, user, true)
	if err != nil {
		return nil, err
	}

	var parsed tagResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Some models still wrap JSON in code fences despite format=json. Try
		// to recover before giving up.
		if recovered := extractJSON(raw); recovered != "" {
			if err2 := json.Unmarshal([]byte(recovered), &parsed); err2 == nil {
				return normalizeTags(parsed.Tags), nil
			}
		}
		return nil, fmt.Errorf("ollama: unable to parse tag response: %w (raw=%q)", err, raw)
	}

	return normalizeTags(parsed.Tags), nil
}

func normalizeTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		t = strings.ReplaceAll(t, " ", "-")
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// extractJSON pulls out the first {...} block from a string. Best-effort
// recovery for models that ignore format=json.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}
