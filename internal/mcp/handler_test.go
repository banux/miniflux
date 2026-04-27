// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// callRPC is a small driver that sends a body to (h *handler).serveJSONRPC
// and returns the decoded response. The handler doesn't touch storage for
// initialize / tools/list / ping / unknown method, so we can pass nil here.
func callRPC(t *testing.T, body string) jsonrpcResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	(&handler{store: nil}).serveJSONRPC(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected JSON content type, got %q (status %d, body %q)", ct, rec.Code, rec.Body.String())
	}
	var resp jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (raw=%q)", err, rec.Body.String())
	}
	return resp
}

func TestServeJSONRPCRejectsMalformedJSON(t *testing.T) {
	resp := callRPC(t, "not-json")
	if resp.Error == nil || resp.Error.Code != errParseError {
		t.Fatalf("expected parse error, got %+v", resp.Error)
	}
}

func TestServeJSONRPCRejectsWrongVersion(t *testing.T) {
	resp := callRPC(t, `{"jsonrpc":"1.0","id":1,"method":"ping"}`)
	if resp.Error == nil || resp.Error.Code != errInvalidRequest {
		t.Fatalf("expected invalid request, got %+v", resp.Error)
	}
}

func TestInitializeReturnsServerInfoAndCapabilities(t *testing.T) {
	resp := callRPC(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	body, _ := json.Marshal(resp.Result)
	var parsed initializeResult
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if parsed.ServerInfo.Name != "miniflux" {
		t.Errorf("unexpected server name: %q", parsed.ServerInfo.Name)
	}
	if parsed.ProtocolVersion != protocolVersion {
		t.Errorf("unexpected protocol version: %q", parsed.ProtocolVersion)
	}
	if parsed.Capabilities.Tools == nil {
		t.Error("expected tools capability to be advertised")
	}
}

func TestToolsListReturnsCatalog(t *testing.T) {
	resp := callRPC(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	body, _ := json.Marshal(resp.Result)
	var parsed toolsListResult
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(parsed.Tools) == 0 {
		t.Fatal("expected at least one tool in the catalog")
	}
	// Sanity-check a couple of well-known tools are present.
	names := map[string]bool{}
	for _, tl := range parsed.Tools {
		names[tl.Name] = true
		if tl.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tl.Name)
		}
	}
	for _, want := range []string{"list_unread_entries", "search_entries", "get_entry", "mark_entry_read"} {
		if !names[want] {
			t.Errorf("tool catalog missing %q", want)
		}
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	resp := callRPC(t, `{"jsonrpc":"2.0","id":3,"method":"does_not_exist"}`)
	if resp.Error == nil || resp.Error.Code != errMethodNotFound {
		t.Fatalf("expected method not found, got %+v", resp.Error)
	}
}

func TestNotificationProducesNoBody(t *testing.T) {
	// Notifications (no id) must yield a 204 with empty body.
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	rec := httptest.NewRecorder()
	(&handler{store: nil}).serveJSONRPC(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body, got %q", rec.Body.String())
	}
}

func TestPingReturnsEmptyResult(t *testing.T) {
	resp := callRPC(t, `{"jsonrpc":"2.0","id":4,"method":"ping"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
}

func TestToolCallUnknownToolFailsAsMethodNotFound(t *testing.T) {
	// We use tools/call dispatch directly on a junk name. The handler
	// returns an MCP method-not-found error rather than a tool error.
	body := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`
	resp := callRPC(t, body)
	if resp.Error == nil || resp.Error.Code != errMethodNotFound {
		t.Fatalf("expected method not found, got %+v", resp.Error)
	}
}

func TestToolCallInvalidParamsFails(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":"oops"}`
	resp := callRPC(t, body)
	if resp.Error == nil || resp.Error.Code != errInvalidParams {
		t.Fatalf("expected invalid params, got %+v", resp.Error)
	}
}

func TestMissingAPIKeyIsRejected(t *testing.T) {
	// validateAPIKey runs before the handler, so a request with no header
	// gets a JSON-RPC error response without ever touching the store.
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	validateAPIKey(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not run when token is missing")
	})).ServeHTTP(rec, req)

	var resp jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (raw=%q)", err, rec.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != errInvalidRequest {
		t.Fatalf("expected invalid request error, got %+v", resp.Error)
	}
}

func TestRegisteredToolHandlersHaveASchema(t *testing.T) {
	// Catch the "added a handler but forgot to advertise it" mistake.
	if len(toolHandlers) != len(toolList) {
		t.Fatalf("handler/list mismatch: %d handlers vs %d advertised", len(toolHandlers), len(toolList))
	}
	for _, tl := range toolList {
		if _, ok := toolHandlers[tl.Name]; !ok {
			t.Errorf("tool %q advertised but no handler registered", tl.Name)
		}
	}
}
