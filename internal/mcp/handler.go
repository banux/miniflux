// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcp // import "miniflux.app/v2/internal/mcp"

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/version"
)

// NewHandler returns the http.Handler exposing the MCP endpoint at /mcp.
// All requests pass through the API-key middleware first, so handlers below
// can rely on request.UserID(r) being set.
func NewHandler(store *storage.Storage) http.Handler {
	h := &handler{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", h.serveJSONRPC)
	return validateAPIKey(store, mux)
}

type handler struct {
	store *storage.Storage
}

func (h *handler) serveJSONRPC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSONRPCError(w, nil, errInvalidRequest, "request body too large or unreadable")
		return
	}

	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, errParseError, "invalid JSON: "+err.Error())
		return
	}

	if req.JSONRPC != "2.0" {
		writeJSONRPCError(w, req.ID, errInvalidRequest, `jsonrpc field must be "2.0"`)
		return
	}

	result, rpcErr := h.dispatch(r, &req)

	if req.isNotification() {
		// Spec says no response is sent back for notifications, even on error.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	resp := jsonrpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	writeJSON(w, http.StatusOK, &resp)
}

// dispatch routes a parsed JSON-RPC method to the right handler. It returns
// either a result payload or a JSON-RPC error; transport-level issues are
// handled by the caller.
func (h *handler) dispatch(r *http.Request, req *jsonrpcRequest) (any, *jsonrpcError) {
	switch req.Method {
	case "initialize":
		return h.handleInitialize(req), nil
	case "notifications/initialized":
		// Client signals that it has finished initializing. No-op.
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return toolsListResult{Tools: toolCatalog()}, nil
	case "tools/call":
		return h.handleToolCall(r, req)
	default:
		return nil, &jsonrpcError{Code: errMethodNotFound, Message: "method not found: " + req.Method}
	}
}

func (h *handler) handleInitialize(_ *jsonrpcRequest) initializeResult {
	return initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities: capabilities{
			Tools: &toolsCapability{ListChanged: false},
		},
		ServerInfo: implementation{
			Name:    "miniflux",
			Version: version.Version,
		},
	}
}

func (h *handler) handleToolCall(r *http.Request, req *jsonrpcRequest) (any, *jsonrpcError) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &jsonrpcError{Code: errInvalidParams, Message: "invalid tools/call params: " + err.Error()}
	}

	exec, ok := toolHandlers[params.Name]
	if !ok {
		return nil, &jsonrpcError{Code: errMethodNotFound, Message: "unknown tool: " + params.Name}
	}

	out, err := exec(r, h.store, params.Arguments)
	if err != nil {
		// Tool errors flow back as a tools/call result with isError=true so
		// the LLM can react, not as JSON-RPC errors which would surface as
		// transport failures.
		slog.Warn("[MCP] tool execution failed",
			slog.String("tool", params.Name), slog.Any("error", err))
		return toolCallResult{
			Content: []contentBlock{textBlock("error: " + err.Error())},
			IsError: true,
		}, nil
	}
	return out, nil
}

// writeJSON serializes any payload at the given status. Response is best
// effort: a write failure here means the client is gone.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("[MCP] response write failed", slog.Any("error", err))
	}
}

// writeJSONRPCError emits a JSON-RPC error envelope. Used for transport-level
// failures (auth, parse, malformed requests) where we cannot delegate to the
// dispatcher.
func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonrpcError{Code: code, Message: msg},
	}
	writeJSON(w, http.StatusOK, &resp)
}

// errBadArgs is the canonical error returned by tools when the LLM passes
// malformed arguments. Tools wrap their own context on top.
var errBadArgs = errors.New("invalid tool arguments")

// --- In-process client ------------------------------------------------------
//
// The chat agent lives in the same process as the MCP server. Instead of
// looping back over HTTP, it calls these helpers directly. They keep the
// boundary intact: the agent only knows the tool catalog and the call
// surface, never the individual tool implementations.

// CatalogTool is the public representation of a tool exposed to in-process
// callers (chat agent). Mirrors the JSON shape advertised by tools/list.
type CatalogTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// Catalog returns the list of tools the MCP server exposes. The slice is a
// copy: callers are free to filter or reorder it.
func Catalog() []CatalogTool {
	out := make([]CatalogTool, 0, len(toolList))
	for _, t := range toolList {
		out = append(out, CatalogTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return out
}

// CallTool runs a tool in-process with the given user identity. Arguments
// must be a JSON object matching the tool's input schema; `nil` is treated
// as an empty object. The returned content is what the LLM should observe;
// isError flags semantic failures the agent can react to.
func CallTool(r *http.Request, store *storage.Storage, name string, arguments []byte) (content string, isError bool, err error) {
	exec, ok := toolHandlers[name]
	if !ok {
		return "", true, fmt.Errorf("unknown MCP tool: %s", name)
	}
	if r == nil {
		return "", true, errors.New("CallTool: nil request, user context required")
	}

	out, execErr := exec(r, store, arguments)
	if execErr != nil {
		return "error: " + execErr.Error(), true, nil
	}
	if len(out.Content) == 0 {
		return "", out.IsError, nil
	}
	// MCP returns one or more content blocks; for the agent we concatenate
	// the text payloads (in practice, tools return a single TextContent).
	var sb strings.Builder
	for i, block := range out.Content {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(block.Text)
	}
	return sb.String(), out.IsError, nil
}
