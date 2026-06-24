// Package schema defines the OpenAI-compatible request/response types that
// flow through the gateway. Provider adapters translate to and from these.
package schema

import "encoding/json"

// ChatRequest is the OpenAI-compatible /v1/chat/completions request body.
type ChatRequest struct {
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`

	// Passthrough captures any provider-specific fields we don't model so
	// they survive the round trip.
	Passthrough map[string]json.RawMessage `json:"-"`
}

// Message is a single chat turn. Content is kept as RawMessage because the
// OpenAI schema allows either a string or an array of content parts
// (multimodal). The compression layer understands both shapes.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// TextContent returns the message content as plain text when it is a JSON
// string, or the concatenated text parts when it is a multimodal array.
// Returns ("", false) if there is no extractable text.
func (m Message) TextContent() (string, bool) {
	if len(m.Content) == 0 {
		return "", false
	}
	// String form.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, true
	}
	// Array-of-parts form.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		out := ""
		for _, p := range parts {
			if p.Type == "text" {
				out += p.Text
			}
		}
		return out, out != ""
	}
	return "", false
}

// SetText replaces the content with a plain JSON string.
func (m *Message) SetText(s string) {
	b, _ := json.Marshal(s)
	m.Content = b
}

// ToolCall is a parsed function tool call requested by the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded argument string
	} `json:"function"`
}

// ParseToolCalls decodes the message's tool_calls field, if any.
func (m Message) ParseToolCalls() []ToolCall {
	if len(m.ToolCalls) == 0 {
		return nil
	}
	var calls []ToolCall
	if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
		return nil
	}
	return calls
}

// ChatResponse is the non-streaming OpenAI-compatible response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`

	// Frostgate adds gateway metadata under an x_ prefix so it never
	// collides with provider fields and is easy for clients to ignore.
	Gateway *GatewayMeta `json:"x_frostgate,omitempty"`
}

// Choice is one completion alternative.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage reports token accounting.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// GatewayMeta surfaces what the gateway did for a request: which provider
// actually served it, whether it was a cache hit, and how many tokens the
// compression layer saved.
type GatewayMeta struct {
	Provider       string `json:"provider"`
	ResolvedModel  string `json:"resolved_model"`
	CacheHit       bool   `json:"cache_hit,omitempty"`
	Attempts       int    `json:"attempts,omitempty"`
	TokensSaved    int    `json:"tokens_saved,omitempty"`
	CompressionUsed string `json:"compression,omitempty"`
	ToolCalls      int    `json:"tool_calls,omitempty"`   // MCP tool calls executed
	ToolIterations int    `json:"tool_iterations,omitempty"`
	NodeID         string `json:"node,omitempty"`
}
