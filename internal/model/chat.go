// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model // import "miniflux.app/v2/internal/model"

import "time"

// Chat message roles, matching the OpenAI/Ollama-style chat format the agent
// uses internally and the chat_messages.role CHECK constraint in the DB.
const (
	ChatRoleSystem    = "system"
	ChatRoleUser      = "user"
	ChatRoleAssistant = "assistant"
	ChatRoleTool      = "tool"
)

// ChatConversation is a thread of messages between one user and the agent.
type ChatConversation struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Non-persisted: filled when loading a conversation with its messages.
	Messages []*ChatMessage `json:"messages,omitempty"`
}

// ChatMessage is one entry in the conversation. assistant messages can carry
// either content (final answer) or tool_calls (the agent wants to invoke
// tools); tool messages carry the result returned for a previous tool_call.
type ChatMessage struct {
	ID             int64           `json:"id"`
	ConversationID int64           `json:"conversation_id"`
	Role           string          `json:"role"`
	Content        string          `json:"content"`
	ToolCalls      []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// ChatToolCall describes one tool invocation requested by the LLM. Arguments
// are kept as a free-form map to match the JSON shape Ollama emits and to
// pass through to the MCP layer without re-serialising twice.
type ChatToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}
