// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcp // import "miniflux.app/v2/internal/mcp"

import (
	"context"
	"log/slog"
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/storage"
)

// validateAPIKey is a sister of the REST API's middleware: same X-Auth-Token
// header, same userByAPIKey lookup, but we always require a key — the MCP
// endpoint has no anonymous mode.
func validateAPIKey(store *storage.Storage, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := request.ClientIP(r)
		token := r.Header.Get("X-Auth-Token")

		if token == "" {
			slog.Warn("[MCP] Missing API key",
				slog.String("client_ip", clientIP),
				slog.String("user_agent", r.UserAgent()),
				slog.String("request_uri", r.RequestURI),
			)
			writeJSONRPCError(w, nil, errInvalidRequest, "missing API key (set X-Auth-Token)")
			return
		}

		user, err := store.UserByAPIKey(token)
		if err != nil {
			slog.Error("[MCP] API key lookup failed",
				slog.String("client_ip", clientIP),
				slog.Any("error", err),
			)
			writeJSONRPCError(w, nil, errInternalError, "internal error")
			return
		}

		if user == nil {
			slog.Warn("[MCP] Unknown API key",
				slog.Bool("authentication_failed", true),
				slog.String("client_ip", clientIP),
				slog.String("user_agent", r.UserAgent()),
			)
			writeJSONRPCError(w, nil, errInvalidRequest, "invalid API key")
			return
		}

		store.SetLastLogin(user.ID)
		store.SetAPIKeyUsedTimestamp(user.ID, token)

		ctx := r.Context()
		ctx = context.WithValue(ctx, request.UserIDContextKey, user.ID)
		ctx = context.WithValue(ctx, request.UserTimezoneContextKey, user.Timezone)
		ctx = context.WithValue(ctx, request.IsAdminUserContextKey, user.IsAdmin)
		ctx = context.WithValue(ctx, request.IsAuthenticatedContextKey, true)

		slog.Debug("[MCP] Authenticated",
			slog.String("client_ip", clientIP),
			slog.String("username", user.Username),
		)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
