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

// backfillBatchSize is the maximum number of entries the manual backfill
// processes per click. Big enough to make a dent on a backlog, small enough
// that the user can re-click rather than wait for a runaway loop if the
// model becomes unhappy mid-batch.
const backfillBatchSize = 200

// backfillRunning tracks per-user manual backfills so a double-click does
// not spawn two goroutines competing for the same pending entries.
var backfillRunning sync.Map // map[int64]struct{}

// BackfillUser runs Ollama enrichment for one batch of the user's entries
// that still lack a score. Intended to be invoked from a goroutine after the
// UI handler has already responded — failures are logged, never returned.
func BackfillUser(store *storage.Storage, userID int64) {
	if !config.Opts.OllamaEnabled() {
		return
	}
	if _, busy := backfillRunning.LoadOrStore(userID, struct{}{}); busy {
		slog.Info("ollama: backfill skipped, another run is already in progress",
			slog.Int64("user_id", userID))
		return
	}
	defer backfillRunning.Delete(userID)

	entries, err := store.GetEntriesForOllamaBackfill(userID, backfillBatchSize)
	if err != nil {
		slog.Warn("ollama: backfill failed to fetch pending entries",
			slog.Int64("user_id", userID), slog.Any("error", err))
		return
	}
	if len(entries) == 0 {
		slog.Info("ollama: backfill found nothing to do", slog.Int64("user_id", userID))
		return
	}

	groupsByFeed := make(map[int64]model.Entries)
	for _, entry := range entries {
		groupsByFeed[entry.FeedID] = append(groupsByFeed[entry.FeedID], entry)
	}

	slog.Info("ollama: backfill starting",
		slog.Int64("user_id", userID),
		slog.Int("entries", len(entries)),
		slog.Int("feeds", len(groupsByFeed)))

	for feedID, feedEntries := range groupsByFeed {
		feed, err := store.FeedByID(userID, feedID)
		if err != nil || feed == nil {
			slog.Warn("ollama: backfill skipping feed it cannot load",
				slog.Int64("user_id", userID),
				slog.Int64("feed_id", feedID),
				slog.Any("error", err))
			continue
		}
		EnrichEntries(store, feed, feedEntries)
	}
}

// EnrichEntries runs Ollama tag extraction and scoring for the new entries
// produced by a feed refresh. It is meant to be invoked in its own goroutine.
// The function is best-effort: any per-entry failure is logged and skipped so
// that one failing entry does not block the rest.
func EnrichEntries(store *storage.Storage, feed *model.Feed, entries model.Entries) {
	if !config.Opts.OllamaEnabled() || len(entries) == 0 {
		return
	}
	if feed.DisableOllama {
		slog.Debug("ollama: enrichment disabled for this feed",
			slog.Int64("feed_id", feed.ID),
			slog.Int64("user_id", feed.UserID))
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

	stats := batchStats{}
	batchStart := time.Now()
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			slog.Warn("ollama: context expired before all entries were enriched",
				slog.Int64("user_id", feed.UserID),
				slog.Int64("feed_id", feed.ID),
				slog.Int("remaining", len(entries)-stats.processed))
			break
		}
		enrichOne(ctx, client, store, feed, entry, profile, filterEnabled && enoughSamples, threshold, &stats)
	}

	slog.Info("ollama: batch enrichment done",
		slog.Int64("user_id", feed.UserID),
		slog.Int64("feed_id", feed.ID),
		slog.Int("entries", len(entries)),
		slog.Int("processed", stats.processed),
		slog.Int("tag_errors", stats.tagErrors),
		slog.Int("score_errors", stats.scoreErrors),
		slog.Int("filtered", stats.filtered),
		slog.Duration("duration", time.Since(batchStart)))
}

// batchStats captures aggregate counters for one EnrichEntries invocation.
// Kept as a small struct so we never drop a counter update by forgetting to
// thread it through a return value.
type batchStats struct {
	processed   int
	tagErrors   int
	scoreErrors int
	filtered    int
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
	stats *batchStats,
) {
	if !acquireSlot(ctx) {
		return
	}
	defer releaseSlot()
	stats.processed++

	plain := sanitizer.StripTags(entry.Content)
	plain = strings.TrimSpace(plain)
	rescraped := false

	if len(plain) < config.Opts.OllamaMinContentLength() {
		if scraped := scrapeForOllama(feed, entry.URL); scraped != "" {
			plain = scraped
			rescraped = true
		}
	}

	logger := slog.With(
		slog.Int64("user_id", entry.UserID),
		slog.Int64("entry_id", entry.ID),
		slog.String("entry_url", entry.URL),
		slog.Bool("rescraped", rescraped),
	)

	tagsStart := time.Now()
	tags, err := client.ExtractTags(ctx, entry.Title, entry.URL, plain)
	tagsDuration := time.Since(tagsStart)
	if err != nil {
		stats.tagErrors++
		logger.Warn("ollama: tag extraction failed",
			slog.Any("error", err),
			slog.Duration("duration", tagsDuration))
	} else {
		logger.Debug("ollama: tags extracted",
			slog.Int("tag_count", len(tags)),
			slog.Duration("duration", tagsDuration))
	}

	scoreStart := time.Now()
	score, err := client.ScoreEntry(ctx, profile, entry.Title, entry.URL, tags, plain)
	scoreDuration := time.Since(scoreStart)
	if err != nil {
		stats.scoreErrors++
		logger.Warn("ollama: scoring failed",
			slog.Any("error", err),
			slog.Duration("duration", scoreDuration))
		// Persist tags alone if we got them; otherwise nothing to do.
		if len(tags) > 0 {
			if err := store.UpdateEntryOllamaEnrichment(entry.ID, 0, tags); err != nil {
				logger.Warn("ollama: unable to persist tags", slog.Any("error", err))
			}
		}
		return
	}

	logger.Debug("ollama: entry scored",
		slog.Float64("score", score),
		slog.Duration("duration", scoreDuration))

	if err := store.UpdateEntryOllamaEnrichment(entry.ID, score, tags); err != nil {
		logger.Warn("ollama: unable to persist enrichment", slog.Any("error", err))
		return
	}

	if filter && score < threshold {
		stats.filtered++
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
