package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"frostgate/internal/config"
	"frostgate/internal/mcp"
	"frostgate/internal/provider"
	"frostgate/internal/schema"
)

// toolThenAnswerProvider returns a tool call on its first invocation and a
// final answer once it sees a tool result message. This simulates a model
// driving the agentic loop.
type toolThenAnswerProvider struct{ name string }

func (p *toolThenAnswerProvider) Name() string { return p.name }

func (p *toolThenAnswerProvider) Chat(ctx context.Context, key, model string, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	hasToolResult := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			hasToolResult = true
		}
	}
	msg := schema.Message{Role: "assistant"}
	if !hasToolResult {
		// Request a tool call.
		msg.ToolCalls = json.RawMessage(`[{"id":"call_1","type":"function",` +
			`"function":{"name":"add","arguments":"{\"a\":3,\"b\":4}"}}]`)
	} else {
		msg.SetText("the answer is 7")
	}
	return &schema.ChatResponse{
		Object: "chat.completion", Model: model,
		Choices: []schema.Choice{{Index: 0, Message: msg, FinishReason: "stop"}},
		Usage:   schema.Usage{TotalTokens: 5},
	}, nil
}

func (p *toolThenAnswerProvider) Stream(ctx context.Context, key, model string, req *schema.ChatRequest, w io.Writer, flush func()) error {
	return nil
}

func fakeMCP(t *testing.T) *mcp.Manager {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			out["result"] = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			out["result"] = map[string]any{"tools": []map[string]any{
				{"name": "add", "description": "add", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			out["result"] = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "7"}}, "isError": false,
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	m, errs := mcp.NewManager(config.MCPConfig{
		Enabled: true, MaxToolIterations: 4,
		Servers: []config.MCPServer{{Name: "math", Transport: "http", URL: srv.URL}},
	})
	if len(errs) > 0 {
		t.Fatalf("mcp manager errors: %v", errs)
	}
	return m
}

func TestAgenticToolLoop(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{"p": {Kind: "mock", Keys: []config.Key{{Value: "k", Weight: 1}}}},
		Models:    map[string]config.ModelRoute{"agent": {Targets: []config.Target{{Provider: "p", Model: "m"}}}},
		Cache:     config.CacheConfig{Enabled: true, TTLSeconds: 60, MaxEntries: 10},
	}
	providers := map[string]provider.Provider{"p": &toolThenAnswerProvider{name: "p"}}
	g := New(cfg, providers, fakeMCP(t), nil)

	resp, err := g.Complete(context.Background(), userReq("agent", "what is 3+4?"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	txt, _ := resp.Choices[0].Message.TextContent()
	if txt != "the answer is 7" {
		t.Fatalf("expected final answer after tool loop, got %q", txt)
	}
	if resp.Gateway.ToolCalls != 1 {
		t.Fatalf("expected 1 tool call recorded, got %d", resp.Gateway.ToolCalls)
	}
	if resp.Gateway.ToolIterations != 2 {
		t.Fatalf("expected 2 iterations (tool round + final), got %d", resp.Gateway.ToolIterations)
	}
}
