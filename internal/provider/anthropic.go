package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"frostgate/internal/schema"
)

// Anthropic adapts the Anthropic Messages API to our OpenAI-compatible
// schema. The notable differences it bridges: the system prompt is a
// top-level field (not a message), content blocks differ, and max_tokens is
// required.
type Anthropic struct {
	name    string
	baseURL string
	client  *http.Client
	version string
}

// NewAnthropic builds an Anthropic adapter.
func NewAnthropic(name, baseURL string) *Anthropic {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	return &Anthropic{name: name, baseURL: baseURL, client: &http.Client{}, version: "2023-06-01"} // bump when Anthropic releases a newer stable version
}

func (a *Anthropic) Name() string { return a.name }

// anthropicBody is the request shape for /messages.
type anthropicBody struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (a *Anthropic) build(model string, req *schema.ChatRequest, stream bool) (*anthropicBody, error) {
	b := &anthropicBody{Model: model, Stream: stream, Temperature: req.Temperature, TopP: req.TopP}
	if req.MaxTokens != nil {
		b.MaxTokens = *req.MaxTokens
	} else {
		b.MaxTokens = 4096 // Anthropic requires this; pick a sane default.
	}
	for _, m := range req.Messages {
		text, _ := m.TextContent()
		switch m.Role {
		case "system":
			if b.System != "" {
				b.System += "\n\n"
			}
			b.System += text
		case "user", "assistant":
			b.Messages = append(b.Messages, anthropicMessage{Role: m.Role, Content: text})
		default:
			// Fold tool/other roles into a user turn so context survives.
			b.Messages = append(b.Messages, anthropicMessage{Role: "user", Content: text})
		}
	}
	return b, nil
}

func (a *Anthropic) do(ctx context.Context, apiKey string, body *anthropicBody, stream bool) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", a.version)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	return a.client.Do(httpReq)
}

func (a *Anthropic) Chat(ctx context.Context, apiKey, model string, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	body, err := a.build(model, req, false)
	if err != nil {
		return nil, err
	}
	resp, err := a.do(ctx, apiKey, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, HTTPErrorFromResp(resp)
	}
	raw, _ := io.ReadAll(resp.Body)
	// Parse Anthropic's response and map to OpenAI shape.
	var ar struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	var text strings.Builder
	for _, c := range ar.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	msg := schema.Message{Role: "assistant"}
	msg.SetText(text.String())
	return &schema.ChatResponse{
		ID:      ar.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   ar.Model,
		Choices: []schema.Choice{{Index: 0, Message: msg, FinishReason: mapStop(ar.StopReason)}},
		Usage: schema.Usage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}, nil
}

func mapStop(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	return r
}

func (a *Anthropic) Stream(ctx context.Context, apiKey, model string, req *schema.ChatRequest, w io.Writer, flush func()) error {
	body, err := a.build(model, req, true)
	if err != nil {
		return err
	}
	resp, err := a.do(ctx, apiKey, body, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return HTTPErrorFromResp(resp)
	}
	// Translate Anthropic SSE events into OpenAI-style chat.completion.chunk
	// deltas so clients see a uniform stream regardless of provider.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	id := "chatcmpl-stream"
	created := time.Now().Unix()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta.Text != "" {
			chunk := openAIChunk(id, created, model, ev.Delta.Text, "")
			if err := writeSSE(w, chunk); err != nil {
				return err
			}
			flush()
		}
		if ev.Type == "message_stop" {
			chunk := openAIChunk(id, created, model, "", "stop")
			_ = writeSSE(w, chunk)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flush()
		}
	}
	return sc.Err()
}

// openAIChunk builds a chat.completion.chunk delta object.
func openAIChunk(id string, created int64, model, content, finish string) map[string]any {
	delta := map[string]any{}
	if content != "" {
		delta["content"] = content
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created,
		"model": model, "choices": []any{choice},
	}
}

func writeSSE(w io.Writer, obj any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}
