// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ollama // import "miniflux.app/v2/internal/integration/ollama"

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const scoreSystemPrompt = `You are a content recommender for a personal RSS reader.
You are given a profile of the user (positive examples they liked and negative examples they actively dismissed) and a candidate article.
Your job is to estimate how interested the user would be in the candidate article, on a scale from 0.0 (not interested at all) to 1.0 (very interested).
Be calibrated: average articles should score around 0.5, only articles that closely match the positive profile should score above 0.7. Penalise candidates that look like the negative examples — they are explicit dismissals from this same user, not noise.
Reply with a JSON object {"score": <float between 0 and 1>} and nothing else.`

type scoreResponse struct {
	Score float64 `json:"score"`
}

// ProfileSample is the projection of a past entry shown to the scorer to
// describe the user's taste. Kept tiny on purpose: most chat models choke when
// the profile dominates the context.
//
// Feedback carries the explicit thumb the user pressed on the entry:
//   - +1 means the user actively boosted this entry (strongest positive signal)
//   - -1 means the user actively dismissed it (strongest negative signal)
//   - 0 falls back to starred/read (implicit positive)
type ProfileSample struct {
	Title    string
	Tags     []string
	Starred  bool
	Feedback int
}

// ScoreEntry asks the model to rate how interesting the candidate article is
// for a user with the given profile. Returns a value clamped to [0, 1].
func (c *Client) ScoreEntry(ctx context.Context, profile []ProfileSample, title, url string, tags []string, contentExcerpt string) (float64, error) {
	if len(profile) == 0 {
		return 0.5, nil
	}

	// Bucket samples by signal strength so the prompt makes the polarity
	// obvious to the model (positive vs negative examples).
	var positives, negatives []ProfileSample
	for _, s := range profile {
		switch {
		case s.Feedback < 0:
			negatives = append(negatives, s)
		default:
			// Feedback >= 0: explicit positive (+1), starred or read entries.
			positives = append(positives, s)
		}
	}

	var b strings.Builder
	if len(positives) > 0 {
		b.WriteString("Positive examples (the user liked these):\n")
		for _, s := range positives {
			marker := "read"
			if s.Feedback > 0 {
				marker = "thumbs-up"
			} else if s.Starred {
				marker = "starred"
			}
			fmt.Fprintf(&b, "- [%s] %s", marker, s.Title)
			if len(s.Tags) > 0 {
				fmt.Fprintf(&b, " (tags: %s)", strings.Join(s.Tags, ", "))
			}
			b.WriteByte('\n')
		}
	}
	if len(negatives) > 0 {
		b.WriteString("\nNegative examples (the user actively dismissed these — penalise candidates that look similar):\n")
		for _, s := range negatives {
			fmt.Fprintf(&b, "- [thumbs-down] %s", s.Title)
			if len(s.Tags) > 0 {
				fmt.Fprintf(&b, " (tags: %s)", strings.Join(s.Tags, ", "))
			}
			b.WriteByte('\n')
		}
	}

	b.WriteString("\nCandidate article:\n")
	fmt.Fprintf(&b, "Title: %s\nURL: %s\n", title, url)
	if len(tags) > 0 {
		fmt.Fprintf(&b, "Tags: %s\n", strings.Join(tags, ", "))
	}
	if contentExcerpt != "" {
		fmt.Fprintf(&b, "Excerpt: %s\n", truncate(contentExcerpt, 1500))
	}

	raw, err := c.chat(ctx, scoreSystemPrompt, b.String(), true)
	if err != nil {
		return 0, err
	}

	var parsed scoreResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		if recovered := extractJSON(raw); recovered != "" {
			if err2 := json.Unmarshal([]byte(recovered), &parsed); err2 != nil {
				return 0, fmt.Errorf("ollama: unable to parse score response: %w (raw=%q)", err2, raw)
			}
		} else {
			return 0, fmt.Errorf("ollama: unable to parse score response: %w (raw=%q)", err, raw)
		}
	}

	return clamp01(parsed.Score), nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
