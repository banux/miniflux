// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcp // import "miniflux.app/v2/internal/mcp"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"strings"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/proxyrotator"
	feedhandler "miniflux.app/v2/internal/reader/handler"
	"miniflux.app/v2/internal/reader/fetcher"
	"miniflux.app/v2/internal/reader/sanitizer"
	"miniflux.app/v2/internal/reader/scraper"
	"miniflux.app/v2/internal/reader/subscription"
	"miniflux.app/v2/internal/storage"

	"github.com/PuerkitoBio/goquery"
)

// toolHandler is the function signature every MCP tool implements. The
// http.Request carries the authenticated user context; store is the shared
// storage; args is the raw JSON the LLM produced for the tool call.
type toolHandler func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error)

// toolHandlers and toolCatalog are kept in sync by the registerTool helper:
// adding a tool in one place wires up both the schema (advertised in
// tools/list) and the executor (used by tools/call).
var (
	toolHandlers = map[string]toolHandler{}
	toolList     []tool
)

func registerTool(t tool, h toolHandler) {
	toolHandlers[t.Name] = h
	toolList = append(toolList, t)
}

// toolCatalog returns the advertised tool list, copy-on-read so the caller
// cannot mutate our internal slice.
func toolCatalog() []tool {
	out := make([]tool, len(toolList))
	copy(out, toolList)
	return out
}

const defaultEntryLimit = 25
const maxEntryLimit = 100

func init() {
	registerTool(tool{
		Name:        "list_unread_entries",
		Description: "List the authenticated user's unread entries, newest first. Use the limit/offset arguments to page through.",
		InputSchema: paginatedSchema("Maximum number of entries to return (default 25, max 100).", "Pagination offset (default 0)."),
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p paginatedArgs
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		builder := store.NewEntryQueryBuilder(request.UserID(r))
		builder.WithStatus(model.EntryStatusUnread)
		builder.WithSorting("published_at", "DESC")
		builder.WithLimit(p.clampLimit())
		builder.WithOffset(p.Offset)
		builder.WithoutContent()
		entries, err := builder.GetEntries()
		if err != nil {
			return toolCallResult{}, err
		}
		return jsonResult(entriesProjection(entries))
	})

	registerTool(tool{
		Name:        "list_starred_entries",
		Description: "List the authenticated user's starred entries, newest first.",
		InputSchema: paginatedSchema("Maximum number of entries to return (default 25, max 100).", "Pagination offset (default 0)."),
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p paginatedArgs
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		builder := store.NewEntryQueryBuilder(request.UserID(r))
		builder.WithStarred(true)
		builder.WithSorting("published_at", "DESC")
		builder.WithLimit(p.clampLimit())
		builder.WithOffset(p.Offset)
		builder.WithoutContent()
		entries, err := builder.GetEntries()
		if err != nil {
			return toolCallResult{}, err
		}
		return jsonResult(entriesProjection(entries))
	})

	registerTool(tool{
		Name:        "search_entries",
		Description: "Full-text search across the authenticated user's entries.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of results (default 25, max 100).",
				},
			},
			"required": []string{"query"},
		},
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		if p.Query == "" {
			return toolCallResult{}, fmt.Errorf("%w: query is required", errBadArgs)
		}
		limit := p.Limit
		if limit <= 0 {
			limit = defaultEntryLimit
		}
		if limit > maxEntryLimit {
			limit = maxEntryLimit
		}
		builder := store.NewEntryQueryBuilder(request.UserID(r))
		builder.WithSearchQuery(p.Query)
		builder.WithLimit(limit)
		builder.WithoutContent()
		entries, err := builder.GetEntries()
		if err != nil {
			return toolCallResult{}, err
		}
		return jsonResult(entriesProjection(entries))
	})

	registerTool(tool{
		Name:        "get_entry",
		Description: "Fetch a single entry by ID, including its full HTML content. The entry must belong to the authenticated user.",
		InputSchema: idSchema("entry_id", "Numeric ID of the entry."),
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p struct {
			EntryID int64 `json:"entry_id"`
		}
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		if p.EntryID == 0 {
			return toolCallResult{}, fmt.Errorf("%w: entry_id is required", errBadArgs)
		}
		builder := store.NewEntryQueryBuilder(request.UserID(r))
		builder.WithEntryID(p.EntryID)
		entry, err := builder.GetEntry()
		if err != nil {
			return toolCallResult{}, err
		}
		if entry == nil {
			return toolCallResult{}, fmt.Errorf("entry %d not found", p.EntryID)
		}
		return jsonResult(entryDetail(entry))
	})

	registerTool(tool{
		Name:        "mark_entry_read",
		Description: "Mark an entry as read.",
		InputSchema: idSchema("entry_id", "Numeric ID of the entry to mark as read."),
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		return setEntryStatus(r, store, args, model.EntryStatusRead)
	})

	registerTool(tool{
		Name:        "mark_entry_unread",
		Description: "Mark an entry as unread.",
		InputSchema: idSchema("entry_id", "Numeric ID of the entry to mark as unread."),
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		return setEntryStatus(r, store, args, model.EntryStatusUnread)
	})

	registerTool(tool{
		Name:        "toggle_starred",
		Description: "Toggle the starred state of an entry.",
		InputSchema: idSchema("entry_id", "Numeric ID of the entry."),
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p struct {
			EntryID int64 `json:"entry_id"`
		}
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		if p.EntryID == 0 {
			return toolCallResult{}, fmt.Errorf("%w: entry_id is required", errBadArgs)
		}
		if err := store.ToggleStarred(request.UserID(r), p.EntryID); err != nil {
			return toolCallResult{}, err
		}
		return toolCallResult{Content: []contentBlock{textBlock("ok")}}, nil
	})

	registerTool(tool{
		Name:        "list_feeds",
		Description: "List the authenticated user's feeds.",
		InputSchema: emptySchema(),
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		feeds, err := store.Feeds(request.UserID(r))
		if err != nil {
			return toolCallResult{}, err
		}
		return jsonResult(feedsProjection(feeds))
	})

	registerTool(tool{
		Name:        "list_categories",
		Description: "List the authenticated user's categories.",
		InputSchema: emptySchema(),
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		categories, err := store.Categories(request.UserID(r))
		if err != nil {
			return toolCallResult{}, err
		}
		return jsonResult(categoriesProjection(categories))
	})

	registerTool(tool{
		Name:        "discover_feeds",
		Description: "Given a website URL, discover the RSS/Atom feeds it exposes. Returns a list of {title, url, type} that can be passed straight to subscribe_feed.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"website_url": map[string]any{
					"type":        "string",
					"description": "Homepage or article page URL to scan for feeds.",
				},
			},
			"required": []string{"website_url"},
		},
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p struct {
			WebsiteURL string `json:"website_url"`
		}
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		if strings.TrimSpace(p.WebsiteURL) == "" {
			return toolCallResult{}, fmt.Errorf("%w: website_url is required", errBadArgs)
		}

		rb := fetcher.NewRequestBuilder()
		rb.WithUserAgent("", config.Opts.HTTPClientUserAgent())
		rb.WithTimeout(config.Opts.HTTPClientTimeout())
		rb.WithProxyRotator(proxyrotator.ProxyRotatorInstance)
		rb.WithCustomApplicationProxyURL(config.Opts.HTTPClientProxyURL())

		subs, errWrap := subscription.NewSubscriptionFinder(rb).FindSubscriptions(p.WebsiteURL, "", "")
		if errWrap != nil {
			return toolCallResult{}, fmt.Errorf("discover_feeds: %s", errWrap.Error())
		}
		out := make([]map[string]string, 0, len(subs))
		for _, s := range subs {
			out = append(out, map[string]string{"title": s.Title, "url": s.URL, "type": s.Type})
		}
		return jsonResult(out)
	})

	registerTool(tool{
		Name:        "subscribe_feed",
		Description: "Subscribe the authenticated user to an RSS/Atom feed by its feed URL. If category_id is omitted, the user's first category is used. Returns the created feed (id, title, url).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"feed_url": map[string]any{
					"type":        "string",
					"description": "The feed URL (use discover_feeds first if you only have the website URL).",
				},
				"category_id": map[string]any{
					"type":        "integer",
					"description": "Category ID to put the feed in. Omit to use the user's first category.",
				},
				"crawler": map[string]any{
					"type":        "boolean",
					"description": "Set to true to enable scraper rules / readability for short feed contents.",
				},
			},
			"required": []string{"feed_url"},
		},
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p struct {
			FeedURL    string `json:"feed_url"`
			CategoryID int64  `json:"category_id"`
			Crawler    bool   `json:"crawler"`
		}
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		if strings.TrimSpace(p.FeedURL) == "" {
			return toolCallResult{}, fmt.Errorf("%w: feed_url is required", errBadArgs)
		}

		userID := request.UserID(r)
		if p.CategoryID == 0 {
			cat, err := store.FirstCategory(userID)
			if err != nil {
				return toolCallResult{}, fmt.Errorf("subscribe_feed: lookup default category: %w", err)
			}
			if cat == nil {
				return toolCallResult{}, fmt.Errorf("subscribe_feed: user has no category, create one first with create_category")
			}
			p.CategoryID = cat.ID
		} else if !store.CategoryIDExists(userID, p.CategoryID) {
			return toolCallResult{}, fmt.Errorf("subscribe_feed: category %d not found for this user", p.CategoryID)
		}

		feed, errWrap := feedhandler.CreateFeed(store, userID, &model.FeedCreationRequest{
			FeedURL:    p.FeedURL,
			CategoryID: p.CategoryID,
			Crawler:    p.Crawler,
		})
		if errWrap != nil {
			return toolCallResult{}, fmt.Errorf("subscribe_feed: %s", errWrap.Error())
		}
		return jsonResult(map[string]any{
			"id":          feed.ID,
			"title":       feed.Title,
			"feed_url":    feed.FeedURL,
			"site_url":    feed.SiteURL,
			"category_id": feed.Category.ID,
		})
	})

	registerTool(tool{
		Name:        "create_category",
		Description: "Create a new category for the authenticated user.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{
					"type":        "string",
					"description": "Display name of the new category.",
				},
			},
			"required": []string{"title"},
		},
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p struct {
			Title string `json:"title"`
		}
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		title := strings.TrimSpace(p.Title)
		if title == "" {
			return toolCallResult{}, fmt.Errorf("%w: title is required", errBadArgs)
		}
		cat, err := store.CreateCategory(request.UserID(r), &model.CategoryCreationRequest{Title: title})
		if err != nil {
			return toolCallResult{}, err
		}
		return jsonResult(categorySummary{ID: cat.ID, Title: cat.Title})
	})

	registerTool(tool{
		Name:        "fetch_url",
		Description: "Download a public web page by URL and return its readable text. Useful when the user asks you to look up content on the open web.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The absolute http(s) URL to fetch.",
				},
			},
			"required": []string{"url"},
		},
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p struct {
			URL string `json:"url"`
		}
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		text, err := fetchURLAsText(r.Context(), p.URL)
		if err != nil {
			return toolCallResult{}, err
		}
		return toolCallResult{Content: []contentBlock{textBlock(text)}}, nil
	})

	registerTool(tool{
		Name:        "web_search",
		Description: "Search the open web (DuckDuckGo HTML endpoint) and return the top results. Useful when the user wants to discover new sources.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max number of results (default 5, max 10).",
				},
			},
			"required": []string{"query"},
		},
	}, func(r *http.Request, store *storage.Storage, args json.RawMessage) (toolCallResult, error) {
		var p struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := decodeArgs(args, &p); err != nil {
			return toolCallResult{}, err
		}
		if strings.TrimSpace(p.Query) == "" {
			return toolCallResult{}, fmt.Errorf("%w: query is required", errBadArgs)
		}
		if p.Limit <= 0 {
			p.Limit = 5
		}
		if p.Limit > 10 {
			p.Limit = 10
		}
		results, err := webSearch(r.Context(), p.Query, p.Limit)
		if err != nil {
			return toolCallResult{}, err
		}
		return jsonResult(results)
	})
}

// fetchURLAsText downloads the URL via the existing miniflux fetcher (which
// already enforces SSRF protection through BlockPrivateNetworks) and returns
// the readable text content. We strictly require an absolute http/https URL
// to avoid the LLM passing in file:// or javascript: schemes.
func fetchURLAsText(ctx context.Context, rawURL string) (string, error) {
	parsed, err := nurl.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported URL scheme %q (only http/https)", parsed.Scheme)
	}

	rb := fetcher.NewRequestBuilder()
	rb.WithUserAgent("", config.Opts.HTTPClientUserAgent())
	rb.WithTimeout(config.Opts.HTTPClientTimeout())
	rb.WithProxyRotator(proxyrotator.ProxyRotatorInstance)
	rb.WithCustomApplicationProxyURL(config.Opts.HTTPClientProxyURL())

	_, content, err := scraper.ScrapeWebsite(rb, rawURL, "")
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", rawURL, err)
	}

	text := strings.TrimSpace(sanitizer.StripTags(content))
	const maxLen = 12000
	if len(text) > maxLen {
		text = text[:maxLen] + "\n…(truncated)"
	}
	if text == "" {
		return "(empty page)", nil
	}
	_ = ctx
	return text, nil
}

type webSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// webSearch hits the DuckDuckGo HTML endpoint (no JS required, no API key)
// and parses the result list. The endpoint is rate-limited but fine for the
// occasional agent query — heavier usage would warrant a real search API.
func webSearch(ctx context.Context, query string, limit int) ([]webSearchResult, error) {
	endpoint := "https://duckduckgo.com/html/?q=" + nurl.QueryEscape(query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", config.Opts.HTTPClientUserAgent())

	client := &http.Client{Timeout: config.Opts.HTTPClientTimeout()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("web_search: ddg %d: %s", resp.StatusCode, string(body))
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("web_search: parse html: %w", err)
	}

	out := make([]webSearchResult, 0, limit)
	doc.Find("div.result").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		titleA := s.Find("a.result__a").First()
		if titleA.Length() == 0 {
			return true
		}
		href, _ := titleA.Attr("href")
		// DDG returns redirect URLs like //duckduckgo.com/l/?uddg=<encoded>.
		// Decode if present so the LLM gets the real link.
		if u, err := nurl.Parse(href); err == nil {
			if real := u.Query().Get("uddg"); real != "" {
				if decoded, derr := nurl.QueryUnescape(real); derr == nil {
					href = decoded
				}
			}
		}
		out = append(out, webSearchResult{
			Title:   strings.TrimSpace(titleA.Text()),
			URL:     href,
			Snippet: strings.TrimSpace(s.Find(".result__snippet").Text()),
		})
		return len(out) < limit
	})
	return out, nil
}

// setEntryStatus is the shared body of mark_entry_read / mark_entry_unread.
func setEntryStatus(r *http.Request, store *storage.Storage, args json.RawMessage, status string) (toolCallResult, error) {
	var p struct {
		EntryID int64 `json:"entry_id"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return toolCallResult{}, err
	}
	if p.EntryID == 0 {
		return toolCallResult{}, fmt.Errorf("%w: entry_id is required", errBadArgs)
	}
	if err := store.SetEntriesStatus(request.UserID(r), []int64{p.EntryID}, status); err != nil {
		return toolCallResult{}, err
	}
	return toolCallResult{Content: []contentBlock{textBlock("ok")}}, nil
}

// paginatedArgs is shared by the list_* tools.
type paginatedArgs struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

func (p paginatedArgs) clampLimit() int {
	limit := p.Limit
	if limit <= 0 {
		return defaultEntryLimit
	}
	if limit > maxEntryLimit {
		return maxEntryLimit
	}
	return limit
}

// decodeArgs unmarshals tool arguments. Empty payloads are tolerated so a
// tool with all-optional inputs can be called with `{}` or no `arguments`.
func decodeArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %v", errBadArgs, err)
	}
	return nil
}

// jsonResult serializes payload as a JSON text content block. The MCP spec
// allows one or more content blocks; a single JSON dump is the simplest shape
// LLMs can consume reliably.
func jsonResult(payload any) (toolCallResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return toolCallResult{}, err
	}
	return toolCallResult{Content: []contentBlock{textBlock(string(body))}}, nil
}

// --- Schema helpers ---------------------------------------------------------

func paginatedSchema(limitDesc, offsetDesc string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit":  map[string]any{"type": "integer", "description": limitDesc},
			"offset": map[string]any{"type": "integer", "description": offsetDesc},
		},
	}
}

func idSchema(name, desc string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			name: map[string]any{"type": "integer", "description": desc},
		},
		"required": []string{name},
	}
}

func emptySchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// --- Projections ------------------------------------------------------------
//
// We don't return the full model entities to the LLM: most fields (timestamps
// in odd timezones, scraper rules, internal flags) are noise, and a smaller
// JSON keeps the agent's context window healthy. Only fields useful for
// downstream tool calls are exposed.

type entrySummary struct {
	ID          int64    `json:"id"`
	FeedID      int64    `json:"feed_id"`
	FeedTitle   string   `json:"feed_title"`
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Author      string   `json:"author,omitempty"`
	PublishedAt string   `json:"published_at"`
	Status      string   `json:"status"`
	Starred     bool     `json:"starred"`
	Tags        []string `json:"tags,omitempty"`
	OllamaScore *float64 `json:"ollama_score,omitempty"`
}

type entryFull struct {
	entrySummary
	Content string `json:"content"`
}

func entriesProjection(entries model.Entries) []entrySummary {
	out := make([]entrySummary, 0, len(entries))
	for _, e := range entries {
		out = append(out, summarize(e))
	}
	return out
}

func entryDetail(e *model.Entry) entryFull {
	return entryFull{
		entrySummary: summarize(e),
		Content:      e.Content,
	}
}

func summarize(e *model.Entry) entrySummary {
	s := entrySummary{
		ID:          e.ID,
		FeedID:      e.FeedID,
		Title:       e.Title,
		URL:         e.URL,
		Author:      e.Author,
		PublishedAt: e.Date.UTC().Format("2006-01-02T15:04:05Z"),
		Status:      e.Status,
		Starred:     e.Starred,
		Tags:        e.Tags,
		OllamaScore: e.OllamaScore,
	}
	if e.Feed != nil {
		s.FeedTitle = e.Feed.Title
	}
	return s
}

type feedSummary struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	SiteURL    string `json:"site_url"`
	FeedURL    string `json:"feed_url"`
	CategoryID int64  `json:"category_id"`
}

func feedsProjection(feeds model.Feeds) []feedSummary {
	out := make([]feedSummary, 0, len(feeds))
	for _, f := range feeds {
		fs := feedSummary{
			ID:      f.ID,
			Title:   f.Title,
			SiteURL: f.SiteURL,
			FeedURL: f.FeedURL,
		}
		if f.Category != nil {
			fs.CategoryID = f.Category.ID
		}
		out = append(out, fs)
	}
	return out
}

type categorySummary struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

func categoriesProjection(cats model.Categories) []categorySummary {
	out := make([]categorySummary, 0, len(cats))
	for _, c := range cats {
		out = append(out, categorySummary{ID: c.ID, Title: c.Title})
	}
	return out
}
