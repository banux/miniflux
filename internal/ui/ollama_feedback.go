// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"errors"
	"net/http"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
)

// setOllamaFeedback applies an explicit user signal on a single entry's
// Ollama score: +1 (boost / clear filter) or -1 (penalize / filter). Posting
// the same direction twice clears the feedback so the entry behaves as a
// regular AI-scored item again.
func (h *handler) setOllamaFeedback(w http.ResponseWriter, r *http.Request) {
	if !config.Opts.OllamaEnabled() {
		response.JSONServerError(w, r, errors.New("ollama: integration disabled"))
		return
	}

	var value int
	switch r.PathValue("direction") {
	case "up":
		value = 1
	case "down":
		value = -1
	default:
		response.JSONBadRequest(w, r, errors.New("ollama: feedback direction must be up or down"))
		return
	}

	entryID := request.RouteInt64Param(r, "entryID")
	resulting, err := h.store.SetOllamaFeedback(request.UserID(r), entryID, value)
	if err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	response.JSON(w, r, map[string]any{
		"feedback": resulting,
	})
}
