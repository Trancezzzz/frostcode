package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"frostgate/internal/schema"
)

// OpenAI adapts any OpenAI-compatible chat API (OpenAI, Azure, Groq, Ollama,
// Together, etc.). The wire format already matches our schema, so this is
// mostly a transport with key injection.
type OpenAI struct {
	name    string
	baseURL string
	client  *http.Client
}

// NewOpenAI builds an OpenAI-compatible adapter. baseURL defaults to the
// public OpenAI endpoint when empty.
func NewOpenAI(name, baseURL string) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAI{
		name:    name,
		baseURL: baseURL,
		client:  &http.Client{Timeout: 0}, // no client timeout; ctx governs
	}
}

func (o *OpenAI) Name() string { return o.name }

func (o *OpenAI) buildBody(model string, req *schema.ChatRequest, stream bool) ([]byte, error) {
	// Marshal the known fields, then splice in any passthrough fields.
	type wire struct {
		Model       string           `json:"model"`
		Messages    []schema.Message `json:"messages"`
		Stream      bool             `json:"stream,omitempty"`
		Temperature *float64         `json:"temperature,omitempty"`
		MaxTokens   *int             `json:"max_tokens,omitempty"`
		TopP        *float64         `json:"top_p,omitempty"`
		Stop        json.RawMessage  `json:"stop,omitempty"`
		Tools       json.RawMessage  `json:"tools,omitempty"`
		ToolChoice  json.RawMessage  `json:"tool_choice,omitempty"`
	}
	w := wire{
		Model: model, Messages: req.Messages, Stream: stream,
		Temperature: req.Temperature, MaxTokens: req.MaxTokens, TopP: req.TopP,
		Stop: req.Stop, Tools: req.Tools, ToolChoice: req.ToolChoice,
	}
	base, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	if len(req.Passthrough) == 0 {
		return base, nil
	}
	// Merge passthrough keys into the object.
	var m map[string]json.RawMessage
	_ = json.Unmarshal(base, &m)
	for k, v := range req.Passthrough {
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

func (o *OpenAI) Chat(ctx context.Context, apiKey, model string, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	body, err := o.buildBody(model, req, false)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(raw)}
	}
	var out schema.ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode openai response: %w", err)
	}
	return &out, nil
}

func (o *OpenAI) Stream(ctx context.Context, apiKey, model string, req *schema.ChatRequest, w io.Writer, flush func()) error {
	body, err := o.buildBody(model, req, true)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return &HTTPError{Status: resp.StatusCode, Body: string(raw)}
	}
	// Pass SSE chunks straight through; the OpenAI stream format already
	// matches what clients expect.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if _, err := w.Write(line); err != nil {
			return err
		}
		_, _ = w.Write([]byte("\n"))
		if len(bytes.TrimSpace(line)) == 0 {
			flush()
		}
	}
	flush()
	return sc.Err()
}

var _ = time.Now // reserved for future retry/backoff timing
