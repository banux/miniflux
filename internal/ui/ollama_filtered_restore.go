// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
)

// restoreOllamaFiltered clears the Ollama filter flag on a single entry,
// putting it back to unread so the user can read or star it.
func (h *handler) restoreOllamaFiltered(w http.ResponseWriter, r *http.Request) {
	entryID := request.RouteInt64Param(r, "entryID")
	if err := h.store.RestoreFilteredEntry(request.UserID(r), entryID); err != nil {
		response.JSONServerError(w, r, err)
		return
	}
	response.JSON(w, r, map[string]string{"message": "restored"})
}
