// Package mcp implements a Model Context Protocol client. The gateway uses it
// to discover tools from MCP servers, advertise them to models in OpenAI tool
// format, and execute tool calls on the model's behalf (agentic loop).
package mcp

import "encoding/json"

// rpcRequest is a JSON-RPC 2.0 request.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// Tool is an MCP tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// listToolsResult is the result of "tools/list".
type listToolsResult struct {
	Tools []Tool `json:"tools"`
}

// callToolResult is the result of "tools/call".
type callToolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// transport carries JSON-RPC calls to one server.
type transport interface {
	call(method string, params any) (json.RawMessage, error)
	close() error
}
