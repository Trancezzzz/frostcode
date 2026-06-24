package provider

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"frostgate/internal/schema"
	"frostgate/internal/tokens"
)

// Mock is a deterministic provider that echoes a summary of the request. It
// needs no API key or network, so it powers tests and a zero-config demo.
type Mock struct{ name string }

// NewMock builds a mock provider.
func NewMock(name string) *Mock { return &Mock{name: name} }

func (m *Mock) Name() string { return m.name }

func (m *Mock) reply(req *schema.ChatRequest) string {
	last := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			last, _ = req.Messages[i].TextContent()
			break
		}
	}
	return fmt.Sprintf("[mock:%s] echo of %d msg(s); last user said: %q", m.name, len(req.Messages), truncate(last, 80))
}

func (m *Mock) Chat(ctx context.Context, apiKey, model string, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	text := m.reply(req)
	msg := schema.Message{Role: "assistant"}
	msg.SetText(text)
	pt := tokens.EstimateMessages(req.Messages)
	ct := tokens.EstimateText(text)
	return &schema.ChatResponse{
		ID: "mock-" + model, Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []schema.Choice{{Index: 0, Message: msg, FinishReason: "stop"}},
		Usage:   schema.Usage{PromptTokens: pt, CompletionTokens: ct, TotalTokens: pt + ct},
	}, nil
}

func (m *Mock) Stream(ctx context.Context, apiKey, model string, req *schema.ChatRequest, w io.Writer, flush func()) error {
	text := m.reply(req)
	id := "mock-stream-" + model
	created := time.Now().Unix()
	for _, word := range strings.Fields(text) {
		if err := writeSSE(w, openAIChunk(id, created, model, word+" ", "")); err != nil {
			return err
		}
		flush()
	}
	_ = writeSSE(w, openAIChunk(id, created, model, "", "stop"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flush()
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
