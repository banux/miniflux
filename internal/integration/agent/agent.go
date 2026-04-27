// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package agent runs a tool-calling chat loop on top of Ollama and the MCP
// server. It is intentionally synchronous: callers wait for one full Run to
// complete and then surface the assistant's final message + the tool trace.
package agent // import "miniflux.app/v2/internal/integration/agent"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/integration/ollama"
	"miniflux.app/v2/internal/mcp"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

// systemPrompt is prepended on every turn. It tells the model what the tools
// can and cannot do, which keeps it from hallucinating IDs or replies.
const systemPrompt = `You are a helpful assistant embedded in the user's Miniflux RSS reader.
You can call the provided tools to read the user's feeds, manage subscriptions, and browse the open web.
Important rules:
- Only act on entries the user explicitly asked about. Do not bulk-mutate.
- Never invent entry IDs, feed IDs, category IDs, or URLs — fetch them through the tools.
- When the user asks about something you cannot answer from the local feeds (looking up new RSS sources, checking facts, reading a specific URL), use web_search and/or fetch_url instead of refusing.
- To add a new subscription: if you only have a website URL, call discover_feeds first to obtain the actual feed URL, then subscribe_feed with that feed URL. Reuse list_categories to pick an existing category before falling back to create_category.
- web_search returns title/url/snippet only; call fetch_url on a result if you need its content.
- When you call a tool, use the exact JSON arguments matching its schema.
- When you have enough information to answer, reply in plain markdown (headings, bold, lists, links allowed), no tool call.
- Answer in the same language as the user.`

// Run executes one user message against the conversation. It loads the
// conversation history, persists the new user message, then loops: ask the
// model, run any tool calls, append the resulting messages, until the model
// produces a final text answer or the step budget is exhausted.
//
// The function only returns an error for transport-level failures
// (DB / Ollama unreachable). Tool errors and budget exhaustion are surfaced
// as assistant messages so the user sees them in the UI.
func Run(ctx context.Context, store *storage.Storage, userID, conversationID int64, userMessage string) error {
	if !config.Opts.ChatEnabled() {
		return errors.New("agent: chat is disabled")
	}

	conv, err := store.ChatConversationByID(userID, conversationID)
	if err != nil {
		return fmt.Errorf("agent: load conversation: %w", err)
	}
	if conv == nil {
		return fmt.Errorf("agent: conversation %d not found for user %d", conversationID, userID)
	}

	// Persist the user message first so that even a downstream failure leaves
	// a coherent trail for the user to see.
	userMsg := &model.ChatMessage{
		ConversationID: conv.ID,
		Role:           model.ChatRoleUser,
		Content:        userMessage,
	}
	if err := store.AppendChatMessage(userMsg); err != nil {
		return fmt.Errorf("agent: persist user message: %w", err)
	}
	conv.Messages = append(conv.Messages, userMsg)

	// Auto-title conversations on first user turn so the list is readable.
	if conv.Title == "" {
		if err := store.SetChatConversationTitle(userID, conv.ID, deriveTitle(userMessage)); err != nil {
			slog.Warn("agent: unable to set title", slog.Int64("conv", conv.ID), slog.Any("error", err))
		}
	}

	client := ollama.NewClient(
		config.Opts.OllamaURL(),
		config.Opts.OllamaModel(),
		config.Opts.ChatTimeout(),
	)

	tools := buildToolCatalog()

	maxSteps := config.Opts.ChatMaxSteps()
	for step := 0; step < maxSteps; step++ {
		messages := buildLLMMessages(conv)

		resp, err := client.ChatWithTools(ctx, messages, tools)
		if err != nil {
			persistErrorAssistant(store, conv, "model error: "+err.Error())
			return fmt.Errorf("agent: chat: %w", err)
		}

		// Model produced a final answer — persist and stop.
		if len(resp.ToolCalls) == 0 {
			final := &model.ChatMessage{
				ConversationID: conv.ID,
				Role:           model.ChatRoleAssistant,
				Content:        resp.Content,
			}
			if err := store.AppendChatMessage(final); err != nil {
				return fmt.Errorf("agent: persist final assistant message: %w", err)
			}
			conv.Messages = append(conv.Messages, final)
			return nil
		}

		// Persist the assistant's tool call request so the UI can render it
		// and the next iteration of the loop can re-send it to the model.
		toolCalls := convertToolCalls(resp.ToolCalls)
		toolReq := &model.ChatMessage{
			ConversationID: conv.ID,
			Role:           model.ChatRoleAssistant,
			Content:        resp.Content,
			ToolCalls:      toolCalls,
		}
		if err := store.AppendChatMessage(toolReq); err != nil {
			return fmt.Errorf("agent: persist tool call request: %w", err)
		}
		conv.Messages = append(conv.Messages, toolReq)

		// Run each tool in sequence and persist the observation.
		for _, call := range toolCalls {
			result := executeTool(ctx, store, userID, call)
			obs := &model.ChatMessage{
				ConversationID: conv.ID,
				Role:           model.ChatRoleTool,
				Content:        result,
				ToolName:       call.Name,
			}
			if err := store.AppendChatMessage(obs); err != nil {
				return fmt.Errorf("agent: persist tool observation: %w", err)
			}
			conv.Messages = append(conv.Messages, obs)
		}
	}

	// Out of steps: emit a fallback so the conversation is not silent.
	persistErrorAssistant(store, conv, fmt.Sprintf("Reached the %d-step budget without a final answer.", maxSteps))
	return nil
}

// buildLLMMessages prepends the system prompt to the persisted history and
// converts our model.ChatMessage to the wire format the Ollama client wants.
func buildLLMMessages(conv *model.ChatConversation) []ollama.AgentMessage {
	out := make([]ollama.AgentMessage, 0, len(conv.Messages)+1)
	out = append(out, ollama.AgentMessage{Role: model.ChatRoleSystem, Content: systemPrompt})
	for _, m := range conv.Messages {
		om := ollama.AgentMessage{
			Role:     m.Role,
			Content:  m.Content,
			ToolName: m.ToolName,
		}
		if len(m.ToolCalls) > 0 {
			om.ToolCalls = make([]ollama.AgentToolCall, len(m.ToolCalls))
			for i, c := range m.ToolCalls {
				om.ToolCalls[i] = ollama.AgentToolCall{
					Function: ollama.AgentToolFunction{Name: c.Name, Arguments: c.Arguments},
				}
			}
		}
		out = append(out, om)
	}
	return out
}

// buildToolCatalog asks the MCP server for its catalog and reshapes it into
// the Ollama wire format. This is recomputed on every Run because the cost
// is negligible and avoids a stale cache when tools evolve.
func buildToolCatalog() []ollama.AgentTool {
	cat := mcp.Catalog()
	out := make([]ollama.AgentTool, 0, len(cat))
	for _, t := range cat {
		out = append(out, ollama.AgentTool{
			Type: "function",
			Function: ollama.AgentToolFunctionSpec{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

// convertToolCalls extracts the model's tool calls into our persisted shape.
func convertToolCalls(in []ollama.AgentToolCall) []model.ChatToolCall {
	out := make([]model.ChatToolCall, 0, len(in))
	for _, c := range in {
		out = append(out, model.ChatToolCall{
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
		})
	}
	return out
}

// executeTool calls into the MCP layer with a synthesised request that
// carries the user identity. The reply is formatted as a plain string for
// the model to ingest as the next observation.
func executeTool(ctx context.Context, store *storage.Storage, userID int64, call model.ChatToolCall) string {
	args, err := json.Marshal(call.Arguments)
	if err != nil {
		return fmt.Sprintf("tool argument marshal error: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(authedContext(ctx, userID))
	content, isError, err := mcp.CallTool(r, store, call.Name, args)
	if err != nil {
		return "transport error: " + err.Error()
	}
	if isError {
		return content
	}
	return content
}

// authedContext layers the user identity on top of the caller context so MCP
// tools can read it via request.UserID. Mirrors what validateAPIKey installs
// on real HTTP requests.
func authedContext(parent context.Context, userID int64) context.Context {
	ctx := parent
	ctx = context.WithValue(ctx, request.UserIDContextKey, userID)
	ctx = context.WithValue(ctx, request.IsAuthenticatedContextKey, true)
	return ctx
}

func persistErrorAssistant(store *storage.Storage, conv *model.ChatConversation, message string) {
	msg := &model.ChatMessage{
		ConversationID: conv.ID,
		Role:           model.ChatRoleAssistant,
		Content:        message,
	}
	if err := store.AppendChatMessage(msg); err != nil {
		slog.Warn("agent: unable to persist error assistant message", slog.Any("error", err))
		return
	}
	conv.Messages = append(conv.Messages, msg)
}

// deriveTitle picks a short, human-friendly title from the first user message.
// It is intentionally rough — anything more elaborate would deserve another
// LLM call which is overkill here.
func deriveTitle(msg string) string {
	const maxRunes = 60
	r := []rune(msg)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return msg
}
