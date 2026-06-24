package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"frostgate/internal/config"
)

// fakeMCPServer is a minimal JSON-RPC MCP server over HTTP for tests. It
// advertises one "add" tool and implements tools/call for it.
func fakeMCPServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("server decode: %v", err)
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = json.RawMessage(`{"protocolVersion":"2024-11-05"}`)
		case "tools/list":
			resp.Result = json.RawMessage(`{"tools":[{"name":"add",` +
				`"description":"add two numbers",` +
				`"inputSchema":{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}}}}]}`)
		case "tools/call":
			// Echo a fixed result; argument parsing is exercised by the manager.
			resp.Result = json.RawMessage(`{"content":[{"type":"text","text":"result: 7"}],"isError":false}`)
		default:
			resp.Error = &rpcError{Code: -32601, Message: "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestManagerDiscoversAndCallsTools(t *testing.T) {
	srv := fakeMCPServer(t)
	defer srv.Close()

	cfg := config.MCPConfig{
		Enabled: true, MaxToolIterations: 3,
		Servers: []config.MCPServer{{Name: "math", Transport: "http", URL: srv.URL}},
	}
	m, errs := NewManager(cfg)
	if len(errs) != 0 {
		t.Fatalf("unexpected manager errors: %v", errs)
	}
	if !m.Enabled() {
		t.Fatalf("manager should be enabled with tools")
	}
	names := m.ToolNames()
	if len(names) != 1 || names[0] != "add" {
		t.Fatalf("expected [add], got %v", names)
	}

	// Tool catalog should be valid OpenAI tool JSON.
	var tools []map[string]any
	if err := json.Unmarshal(m.ToolsJSON(), &tools); err != nil {
		t.Fatalf("tools json invalid: %v", err)
	}
	if tools[0]["type"] != "function" {
		t.Fatalf("expected function tool, got %v", tools[0]["type"])
	}

	out, err := m.Call("add", json.RawMessage(`{"a":3,"b":4}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out != "result: 7" {
		t.Fatalf("unexpected tool output: %q", out)
	}
}

func TestServersSnapshot(t *testing.T) {
	srv := fakeMCPServer(t)
	defer srv.Close()

	cfg := config.MCPConfig{
		Enabled: true,
		Servers: []config.MCPServer{
			{Name: "math", Transport: "http", URL: srv.URL},
			{Name: "broken", Transport: "http", URL: "http://127.0.0.1:0"},
		},
	}
	m, _ := NewManager(cfg)
	defer m.Close()

	servers := m.Servers()
	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}
	// Config order is preserved.
	if servers[0].Name != "math" || servers[1].Name != "broken" {
		t.Fatalf("unexpected order: %v", servers)
	}
	if !servers[0].Connected {
		t.Fatalf("math should be connected")
	}
	if len(servers[0].Tools) != 1 || servers[0].Tools[0] != "add" {
		t.Fatalf("math tools = %v, want [add]", servers[0].Tools)
	}
	if servers[1].Connected {
		t.Fatalf("broken should be disconnected")
	}
	if servers[1].Err == "" {
		t.Fatalf("broken should report an error")
	}
}

func TestReconnect(t *testing.T) {
	srv := fakeMCPServer(t)
	defer srv.Close()

	cfg := config.MCPConfig{
		Enabled: true,
		Servers: []config.MCPServer{{Name: "math", Transport: "http", URL: srv.URL}},
	}
	m, _ := NewManager(cfg)
	defer m.Close()

	if err := m.Reconnect("math"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	// Tools still work after a reconnect.
	if out, err := m.Call("add", json.RawMessage(`{"a":3,"b":4}`)); err != nil || out != "result: 7" {
		t.Fatalf("call after reconnect: out=%q err=%v", out, err)
	}
	if err := m.Reconnect("nope"); err == nil {
		t.Fatalf("expected error reconnecting unknown server")
	}
}

func TestUnknownToolErrors(t *testing.T) {
	m, _ := NewManager(config.MCPConfig{Enabled: true})
	if _, err := m.Call("nope", nil); err == nil {
		t.Fatalf("expected error for unknown tool")
	}
}

func TestDisabledManager(t *testing.T) {
	m, _ := NewManager(config.MCPConfig{Enabled: false})
	if m.Enabled() {
		t.Fatalf("disabled manager should report not enabled")
	}
	if m.ToolsJSON() != nil {
		t.Fatalf("disabled manager should advertise no tools")
	}
}
