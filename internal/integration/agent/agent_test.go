// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"reflect"
	"strings"
	"testing"

	"miniflux.app/v2/internal/integration/ollama"
	"miniflux.app/v2/internal/model"
)

func TestDeriveTitle(t *testing.T) {
	short := "Find me unread tech articles"
	if got := deriveTitle(short); got != short {
		t.Errorf("short message: got %q, want %q", got, short)
	}

	long := strings.Repeat("a", 200)
	got := deriveTitle(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long message: expected ellipsis suffix, got %q", got)
	}
	if rc := len([]rune(got)); rc != 61 {
		// 60 runes of content plus the ellipsis.
		t.Errorf("long message: expected 61 runes, got %d", rc)
	}
}

func TestDeriveTitleHandlesMultiByteRunes(t *testing.T) {
	// 70 emoji should be truncated to 60 + ellipsis without tearing one in half.
	in := strings.Repeat("🦊", 70)
	got := deriveTitle(in)
	if rc := len([]rune(got)); rc != 61 {
		t.Errorf("got %d runes, want 61", rc)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis: %q", got)
	}
}

func TestBuildLLMMessagesPrependsSystemPrompt(t *testing.T) {
	conv := &model.ChatConversation{
		Messages: []*model.ChatMessage{
			{Role: model.ChatRoleUser, Content: "hi"},
			{Role: model.ChatRoleAssistant, Content: "hello"},
		},
	}
	out := buildLLMMessages(conv)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages (system + 2), got %d", len(out))
	}
	if out[0].Role != "system" || !strings.Contains(out[0].Content, "Miniflux") {
		t.Errorf("expected system prompt at index 0, got %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "hi" {
		t.Errorf("user message lost: %+v", out[1])
	}
}

func TestBuildLLMMessagesPropagatesToolCalls(t *testing.T) {
	conv := &model.ChatConversation{
		Messages: []*model.ChatMessage{
			{
				Role: model.ChatRoleAssistant,
				ToolCalls: []model.ChatToolCall{
					{Name: "list_unread_entries", Arguments: map[string]any{"limit": 5}},
				},
			},
			{Role: model.ChatRoleTool, ToolName: "list_unread_entries", Content: "[...]"},
		},
	}
	out := buildLLMMessages(conv)

	asst := out[1]
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(asst.ToolCalls))
	}
	if asst.ToolCalls[0].Function.Name != "list_unread_entries" {
		t.Errorf("wrong tool name: %q", asst.ToolCalls[0].Function.Name)
	}
	if got := asst.ToolCalls[0].Function.Arguments["limit"]; got != 5 {
		t.Errorf("arg lost: %v", got)
	}

	tool := out[2]
	if tool.Role != "tool" || tool.ToolName != "list_unread_entries" || tool.Content != "[...]" {
		t.Errorf("tool message corrupted: %+v", tool)
	}
}

func TestConvertToolCalls(t *testing.T) {
	in := []ollama.AgentToolCall{
		{Function: ollama.AgentToolFunction{Name: "x", Arguments: map[string]any{"k": "v"}}},
	}
	out := convertToolCalls(in)
	if !reflect.DeepEqual(out, []model.ChatToolCall{
		{Name: "x", Arguments: map[string]any{"k": "v"}},
	}) {
		t.Errorf("unexpected conversion: %+v", out)
	}
}

func TestBuildToolCatalogReflectsMCPCatalog(t *testing.T) {
	// Spot-check the wiring: a few well-known MCP tools must be advertised
	// to the LLM.
	tools := buildToolCatalog()
	if len(tools) == 0 {
		t.Fatal("expected at least one tool in agent catalog")
	}
	names := map[string]bool{}
	for _, tool := range tools {
		if tool.Type != "function" {
			continue
		}
		names[tool.Function.Name] = true
		if tool.Function.Parameters == nil {
			t.Errorf("tool %q has no parameters schema", tool.Function.Name)
		}
	}
	for _, want := range []string{"list_unread_entries", "search_entries", "mark_entry_read"} {
		if !names[want] {
			t.Errorf("agent catalog missing tool %q", want)
		}
	}
}
