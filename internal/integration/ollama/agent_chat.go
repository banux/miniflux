// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ollama // import "miniflux.app/v2/internal/integration/ollama"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// AgentMessage is one entry in a multi-turn conversation sent to the LLM.
// It supports the four standard roles (system, user, assistant, tool) and
// optional tool_calls payload that an assistant message can carry when the
// model wants to invoke tools.
type AgentMessage struct {
	Role      string          `json:"role"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []AgentToolCall `json:"tool_calls,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
}

// AgentToolCall describes one tool invocation requested by the LLM.
type AgentToolCall struct {
	Function AgentToolFunction `json:"function"`
}

type AgentToolFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// AgentTool is the catalog entry passed to Ollama in the `tools` array.
type AgentTool struct {
	Type     string                 `json:"type"`
	Function AgentToolFunctionSpec  `json:"function"`
}

type AgentToolFunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// AgentResponse is the single message returned by ChatWithTools. content and
// tool_calls are mutually exclusive in practice — a final answer carries
// content; an in-flight step carries tool_calls.
type AgentResponse struct {
	Content   string
	ToolCalls []AgentToolCall
}

type agentChatRequest struct {
	Model    string         `json:"model"`
	Messages []AgentMessage `json:"messages"`
	Stream   bool           `json:"stream"`
	Tools    []AgentTool    `json:"tools,omitempty"`
	Options  chatOptions    `json:"options,omitempty"`
}

type agentChatResponse struct {
	Message AgentMessage `json:"message"`
	Done    bool         `json:"done"`
}

// ChatWithTools runs one chat turn against /api/chat with tool-calling
// support. messages should already include the system prompt at index 0.
// The same retry policy as the tag/score path applies: one extra attempt on
// transient failures (network, 5xx), nothing on 4xx or decode errors.
func (c *Client) ChatWithTools(ctx context.Context, messages []AgentMessage, tools []AgentTool) (AgentResponse, error) {
	const maxAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := c.doAgentChat(ctx, messages, tools)
		if err == nil {
			return out, nil
		}
		lastErr = err
		var transient *transientError
		if !errors.As(err, &transient) || attempt == maxAttempts {
			return AgentResponse{}, err
		}
		slog.Debug("ollama: transient agent chat error, retrying",
			slog.Int("attempt", attempt), slog.Any("error", err))
		select {
		case <-ctx.Done():
			return AgentResponse{}, ctx.Err()
		case <-time.After(retryBackoff):
		}
	}
	return AgentResponse{}, lastErr
}

func (c *Client) doAgentChat(ctx context.Context, messages []AgentMessage, tools []AgentTool) (AgentResponse, error) {
	payload := agentChatRequest{
		Model:    c.model,
		Messages: messages,
		Stream:   false,
		Tools:    tools,
		Options:  chatOptions{Temperature: 0.3},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return AgentResponse{}, fmt.Errorf("ollama: marshal agent payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return AgentResponse{}, fmt.Errorf("ollama: build agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return AgentResponse{}, &transientError{err: fmt.Errorf("ollama: call /api/chat: %w", err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return AgentResponse{}, &transientError{err: fmt.Errorf("ollama: read agent response: %w", err)}
	}

	if resp.StatusCode != http.StatusOK {
		statusErr := fmt.Errorf("ollama: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		if resp.StatusCode >= 500 {
			return AgentResponse{}, &transientError{err: statusErr}
		}
		return AgentResponse{}, statusErr
	}

	var parsed agentChatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return AgentResponse{}, fmt.Errorf("ollama: decode agent response: %w", err)
	}

	return AgentResponse{
		Content:   parsed.Message.Content,
		ToolCalls: parsed.Message.ToolCalls,
	}, nil
}
