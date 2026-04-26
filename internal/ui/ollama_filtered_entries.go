// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/ui/view"
)

// showOllamaFilteredPage lists entries that the Ollama scorer auto-filtered
// for the current user, so the user can review whether the threshold is set
// appropriately and restore false positives.
func (h *handler) showOllamaFilteredPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	offset := request.QueryIntParam(r, "offset", 0)

	builder := h.store.NewEntryQueryBuilder(user.ID)
	builder.OnlyOllamaFiltered()
	builder.WithSorting("ollama_filtered_at", "DESC")
	builder.WithSorting("id", "DESC")
	builder.WithOffset(offset)
	builder.WithLimit(user.EntriesPerPage)
	builder.WithoutContent()

	entries, count, err := builder.GetEntriesWithCount()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	pending, err := h.store.CountEntriesPendingOllamaEnrichment(user.ID)
	if err != nil {
		pending = 0
	}

	view := view.New(h.tpl, r)
	view.Set("total", count)
	view.Set("entries", entries)
	view.Set("pagination", getPagination(h.routePath("/ollama/filtered"), count, offset, user.EntriesPerPage))
	view.Set("menu", "ollama_filtered")
	view.Set("user", user)
	view.Set("countUnread", h.store.CountUnreadEntries(user.ID))
	view.Set("countErrorFeeds", h.store.CountUserFeedsWithErrors(user.ID))
	view.Set("pendingOllamaEnrichment", pending)

	view.Set("countOllamaFiltered", h.store.CountOllamaFiltered(user.ID))
	view.Set("hasSaveEntry", h.store.HasSaveEntry(user.ID))

	response.HTML(w, r, view.Render("ollama_filtered_entries"))
}
