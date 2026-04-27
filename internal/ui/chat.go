// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/integration/agent"
	"miniflux.app/v2/internal/ui/view"
)

// requireChatEnabled is the small guard every chat handler runs first. The
// menu link is hidden when chat is off, so this is mostly defense-in-depth.
func requireChatEnabled(w http.ResponseWriter, r *http.Request) bool {
	if config.Opts.ChatEnabled() {
		return true
	}
	response.HTMLNotFound(w, r)
	return false
}

func (h *handler) showChatListPage(w http.ResponseWriter, r *http.Request) {
	if !requireChatEnabled(w, r) {
		return
	}
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}
	conversations, err := h.store.ChatConversations(user.ID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	v := view.New(h.tpl, r)
	v.Set("conversations", conversations)
	v.Set("menu", "chat")
	v.Set("user", user)
	v.Set("countUnread", h.store.CountUnreadEntries(user.ID))
	v.Set("countErrorFeeds", h.store.CountUserFeedsWithErrors(user.ID))
	v.Set("countOllamaFiltered", h.store.CountOllamaFiltered(user.ID))
	response.HTML(w, r, v.Render("chat_list"))
}

func (h *handler) showChatConversationPage(w http.ResponseWriter, r *http.Request) {
	if !requireChatEnabled(w, r) {
		return
	}
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}
	convID := request.RouteInt64Param(r, "conversationID")
	conv, err := h.store.ChatConversationByID(user.ID, convID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}
	if conv == nil {
		response.HTMLNotFound(w, r)
		return
	}

	v := view.New(h.tpl, r)
	v.Set("conversation", conv)
	v.Set("menu", "chat")
	v.Set("user", user)
	v.Set("countUnread", h.store.CountUnreadEntries(user.ID))
	v.Set("countErrorFeeds", h.store.CountUserFeedsWithErrors(user.ID))
	v.Set("countOllamaFiltered", h.store.CountOllamaFiltered(user.ID))
	response.HTML(w, r, v.Render("chat_conversation"))
}

func (h *handler) createChatConversation(w http.ResponseWriter, r *http.Request) {
	if !requireChatEnabled(w, r) {
		return
	}
	conv, err := h.store.CreateChatConversation(request.UserID(r), "")
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}
	response.HTMLRedirect(w, r, h.routePath("/chat/%d", conv.ID))
}

func (h *handler) postChatMessage(w http.ResponseWriter, r *http.Request) {
	if !requireChatEnabled(w, r) {
		return
	}
	convID := request.RouteInt64Param(r, "conversationID")
	userMessage := strings.TrimSpace(r.FormValue("message"))
	if userMessage == "" {
		response.HTMLRedirect(w, r, h.routePath("/chat/%d", convID))
		return
	}

	// Run the agent synchronously, with a hard timeout. The agent persists
	// every step (user msg, tool calls, observations, final answer) before
	// we redirect, so the page reload shows the full transcript.
	ctx, cancel := context.WithTimeout(r.Context(), config.Opts.ChatTimeout())
	defer cancel()

	if err := agent.Run(ctx, h.store, request.UserID(r), convID, userMessage); err != nil {
		slog.Warn("chat: agent run failed",
			slog.Int64("conv", convID),
			slog.Int64("user", request.UserID(r)),
			slog.Any("error", err))
		// We still redirect: the user will see the partial transcript and
		// any error message the agent already persisted.
		if errors.Is(err, context.DeadlineExceeded) {
			slog.Info("chat: agent run hit the global timeout", slog.Int64("conv", convID))
		}
	}

	response.HTMLRedirect(w, r, h.routePath("/chat/%d", convID))
}

func (h *handler) deleteChatConversation(w http.ResponseWriter, r *http.Request) {
	if !requireChatEnabled(w, r) {
		response.JSONNotFound(w, r)
		return
	}
	convID := request.RouteInt64Param(r, "conversationID")
	if err := h.store.DeleteChatConversation(request.UserID(r), convID); err != nil {
		response.JSONServerError(w, r, err)
		return
	}
	response.JSON(w, r, map[string]string{"message": "deleted"})
}
