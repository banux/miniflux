// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcp implements a minimal Model Context Protocol server over a
// single HTTP endpoint, authenticated with the existing Miniflux API key.
// The server exposes read and lightweight write tools so an external LLM
// agent can drive a user's Miniflux instance with their API token.
package mcp // import "miniflux.app/v2/internal/mcp"

import (
	"encoding/json"
)

// MCP protocol version this server speaks. Clients announce their own version
// in `initialize`; we do not currently negotiate, the latest spec we built
// against is always returned.
const protocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes used by this server.
const (
	errParseError     = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternalError  = -32603
)

// jsonrpcRequest is a JSON-RPC 2.0 request envelope. Notifications (no id)
// are accepted but never produce a response per the spec.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *jsonrpcRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// initializeResult mirrors the shape MCP clients (Claude Desktop, custom
// agents) expect back from initialize.
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    capabilities   `json:"capabilities"`
	ServerInfo      implementation `json:"serverInfo"`
}

type capabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// tool is one entry in the tools/list catalog. inputSchema is a JSON Schema
// describing the arguments the LLM must produce when calling the tool.
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []tool `json:"tools"`
}

// toolCallParams is the body of a tools/call request.
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toolCallResult is the response shape of tools/call. The MCP spec accepts an
// array of typed content blocks — for now we always return one TextContent
// with a JSON-encoded payload (or a plain string for confirmations).
type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textBlock(s string) contentBlock {
	return contentBlock{Type: "text", Text: s}
}
