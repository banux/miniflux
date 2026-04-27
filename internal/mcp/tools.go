// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcp // import "miniflux.app/v2/internal/mcp"

import (
	"encoding/json"
	"fmt"
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
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
