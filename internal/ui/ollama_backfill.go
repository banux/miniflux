// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"errors"
	"net/http"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/integration/ollama"
)

// triggerOllamaBackfill kicks off a goroutine that scores the next batch of
// entries the user has waiting for enrichment. The HTTP response returns
// immediately — the actual work happens in the background and the user can
// observe progress by reloading the filtered-entries page (which shows the
// pending count).
func (h *handler) triggerOllamaBackfill(w http.ResponseWriter, r *http.Request) {
	if !config.Opts.OllamaEnabled() {
		response.JSONServerError(w, r, errors.New("ollama: integration disabled"))
		return
	}
	go ollama.BackfillUser(h.store, request.UserID(r))
	response.JSON(w, r, map[string]string{"message": "started"})
}
