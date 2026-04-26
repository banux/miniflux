// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ollama // import "miniflux.app/v2/internal/integration/ollama"

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/proxyrotator"
	"miniflux.app/v2/internal/reader/fetcher"
	"miniflux.app/v2/internal/reader/sanitizer"
	"miniflux.app/v2/internal/reader/scraper"
	"miniflux.app/v2/internal/storage"
)

// global concurrency gate, sized once on first use from config.
var (
	semOnce sync.Once
	sem    chan struct{}
)

func acquireSlot(ctx context.Context) bool {
	semOnce.Do(func() {
		sem = make(chan struct{}, max1(config.Opts.OllamaMaxConcurrency()))
	})
	select {
	case sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func releaseSlot() { <-sem }

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

const profileCacheTTL = 5 * time.Minute

type cachedProfile struct {
	samples   []ProfileSample
	expiresAt time.Time
}

var (
	profileCache   sync.Map // map[int64]*cachedProfile
)

// EnrichEntries runs Ollama tag extraction and scoring for the new entries
// produced by a feed refresh. It is meant to be invoked in its own goroutine.
// The function is best-effort: any per-entry failure is logged and skipped so
// that one failing entry does not block the rest.
func EnrichEntries(store *storage.Storage, feed *model.Feed, entries model.Entries) {
	if !config.Opts.OllamaEnabled() || len(entries) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.Opts.OllamaTimeout()*time.Duration(len(entries)+1))
	defer cancel()

	client := NewClient(
		config.Opts.OllamaURL(),
		config.Opts.OllamaModel(),
		config.Opts.OllamaTimeout(),
	)

	profile := loadProfile(store, feed.UserID)
	threshold := float64(config.Opts.OllamaFilterThreshold()) / 100.0
	minSamples := config.Opts.OllamaMinTrainingSamples()
	filterEnabled := threshold > 0
	enoughSamples := false
	if filterEnabled {
		ratedCount, err := store.CountUserRatedEntries(feed.UserID)
		if err != nil {
			slog.Warn("ollama: unable to count rated entries; filtering disabled for this batch",
				slog.Int64("user_id", feed.UserID), slog.Any("error", err))
		}
		enoughSamples = ratedCount >= minSamples
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			slog.Warn("ollama: context expired before all entries were enriched",
				slog.Int64("user_id", feed.UserID), slog.Int("remaining", len(entries)))
			return
		}
		enrichOne(ctx, client, store, feed, entry, profile, filterEnabled && enoughSamples, threshold)
	}
}

func enrichOne(
	ctx context.Context,
	client *Client,
	store *storage.Storage,
	feed *model.Feed,
	entry *model.Entry,
	profile []ProfileSample,
	filter bool,
	threshold float64,
) {
	if !acquireSlot(ctx) {
		return
	}
	defer releaseSlot()

	plain := sanitizer.StripTags(entry.Content)
	plain = strings.TrimSpace(plain)

	if len(plain) < config.Opts.OllamaMinContentLength() {
		if scraped := scrapeForOllama(feed, entry.URL); scraped != "" {
			plain = scraped
		}
	}

	logger := slog.With(
		slog.Int64("user_id", entry.UserID),
		slog.Int64("entry_id", entry.ID),
		slog.String("entry_url", entry.URL),
	)

	tags, err := client.ExtractTags(ctx, entry.Title, entry.URL, plain)
	if err != nil {
		logger.Warn("ollama: tag extraction failed", slog.Any("error", err))
	}

	score, err := client.ScoreEntry(ctx, profile, entry.Title, entry.URL, tags, plain)
	if err != nil {
		logger.Warn("ollama: scoring failed", slog.Any("error", err))
		// Persist tags alone if we got them; otherwise nothing to do.
		if len(tags) > 0 {
			if err := store.UpdateEntryOllamaEnrichment(entry.ID, 0, tags); err != nil {
				logger.Warn("ollama: unable to persist tags", slog.Any("error", err))
			}
		}
		return
	}

	if err := store.UpdateEntryOllamaEnrichment(entry.ID, score, tags); err != nil {
		logger.Warn("ollama: unable to persist enrichment", slog.Any("error", err))
		return
	}

	if filter && score < threshold {
		logger.Info("ollama: filtering entry below threshold",
			slog.Float64("score", score), slog.Float64("threshold", threshold))
		if err := store.MarkEntryAsFiltered(entry.ID); err != nil {
			logger.Warn("ollama: unable to mark filtered entry", slog.Any("error", err))
		}
	}
}

// scrapeForOllama re-fetches the article page when the feed-provided content
// is too short to feed the model meaningfully. Errors are swallowed because
// enrichment must never block the refresh pipeline.
func scrapeForOllama(feed *model.Feed, url string) string {
	rb := fetcher.NewRequestBuilder()
	rb.WithUserAgent(feed.UserAgent, config.Opts.HTTPClientUserAgent())
	rb.WithCookie(feed.Cookie)
	rb.WithTimeout(config.Opts.HTTPClientTimeout())
	rb.WithProxyRotator(proxyrotator.ProxyRotatorInstance)
	rb.WithCustomFeedProxyURL(feed.ProxyURL)
	rb.WithCustomApplicationProxyURL(config.Opts.HTTPClientProxyURL())
	rb.UseCustomApplicationProxyURL(feed.FetchViaProxy)
	rb.IgnoreTLSErrors(feed.AllowSelfSignedCertificates)
	rb.DisableHTTP2(feed.DisableHTTP2)

	_, content, err := scraper.ScrapeWebsite(rb, url, feed.ScraperRules)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(sanitizer.StripTags(content))
}

// loadProfile returns recent appreciation samples for the user, cached briefly
// so that a single refresh batch does not re-query the DB for every entry.
func loadProfile(store *storage.Storage, userID int64) []ProfileSample {
	if cached, ok := profileCache.Load(userID); ok {
		c := cached.(*cachedProfile)
		if time.Now().Before(c.expiresAt) {
			return c.samples
		}
	}

	raw, err := store.GetOllamaUserProfile(userID, 30)
	if err != nil {
		slog.Warn("ollama: unable to load user profile", slog.Int64("user_id", userID), slog.Any("error", err))
		return nil
	}

	samples := make([]ProfileSample, 0, len(raw))
	for _, r := range raw {
		samples = append(samples, ProfileSample{
			Title:   r.Title,
			Tags:    r.Tags,
			Starred: r.Starred,
		})
	}

	profileCache.Store(userID, &cachedProfile{
		samples:   samples,
		expiresAt: time.Now().Add(profileCacheTTL),
	})
	return samples
}
