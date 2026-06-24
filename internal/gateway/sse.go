package gateway

import (
	"bytes"
	"encoding/json"
	"strings"

	"frostgate/internal/schema"
)

// sseAssembler is an io.Writer that consumes an OpenAI-style streaming
// response ("data: {chunk}\n\n" lines) in-process. It surfaces text deltas to
// onText as they arrive and reassembles the full message — including
// index-keyed tool_call fragments — into a schema.ChatResponse, so the agent's
// tool loop works identically to the non-streaming path.
//
// It is used by Gateway.ToolChatStream, which feeds it via Router.Stream /
// Provider.Stream without any change to the Provider interface.
type sseAssembler struct {
	onText      func(string)
	onReasoning func(string)

	buf     bytes.Buffer // accumulates partial writes until newline-delimited
	content strings.Builder
	calls   map[int]*toolCallAccum
	order   []int
	model   string
	id      string
	usage   schema.Usage
	finish  string
}

type toolCallAccum struct {
	id   string
	name string
	args strings.Builder
}

func newSSEAssembler(onText, onReasoning func(string)) *sseAssembler {
	if onText == nil {
		onText = func(string) {}
	}
	if onReasoning == nil {
		onReasoning = func(string) {}
	}
	return &sseAssembler{onText: onText, onReasoning: onReasoning, calls: map[int]*toolCallAccum{}}
}

// Write parses any complete SSE lines contained in p.
func (a *sseAssembler) Write(p []byte) (int, error) {
	a.buf.Write(p)
	for {
		line, err := a.buf.ReadString('\n')
		if err != nil {
			// No full line yet; stash the remainder back for next write.
			a.buf.Reset()
			a.buf.WriteString(line)
			break
		}
		a.handleLine(strings.TrimRight(line, "\r\n"))
	}
	return len(p), nil
}

func (a *sseAssembler) handleLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *schema.Usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return // ignore keep-alives / non-JSON frames
	}
	if chunk.ID != "" {
		a.id = chunk.ID
	}
	if chunk.Model != "" {
		a.model = chunk.Model
	}
	if chunk.Usage != nil {
		a.usage = *chunk.Usage
	}
	for _, ch := range chunk.Choices {
		if rz := ch.Delta.Reasoning + ch.Delta.ReasoningContent; rz != "" {
			a.onReasoning(rz)
		}
		if ch.Delta.Content != "" {
			a.content.WriteString(ch.Delta.Content)
			a.onText(ch.Delta.Content)
		}
		for _, tc := range ch.Delta.ToolCalls {
			acc := a.calls[tc.Index]
			if acc == nil {
				acc = &toolCallAccum{}
				a.calls[tc.Index] = acc
				a.order = append(a.order, tc.Index)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args.WriteString(tc.Function.Arguments)
		}
		if ch.FinishReason != "" {
			a.finish = ch.FinishReason
		}
	}
}

// Response assembles the accumulated stream into a ChatResponse.
func (a *sseAssembler) Response() *schema.ChatResponse {
	msg := schema.Message{Role: "assistant"}
	if a.content.Len() > 0 {
		msg.SetText(a.content.String())
	}
	if len(a.order) > 0 {
		type wireCall struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		calls := make([]wireCall, 0, len(a.order))
		for _, idx := range a.order {
			acc := a.calls[idx]
			var wc wireCall
			wc.ID = acc.id
			wc.Type = "function"
			wc.Function.Name = acc.name
			wc.Function.Arguments = acc.args.String()
			calls = append(calls, wc)
		}
		if b, err := json.Marshal(calls); err == nil {
			msg.ToolCalls = b
		}
	}
	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	return &schema.ChatResponse{
		ID: a.id, Object: "chat.completion", Model: a.model,
		Choices: []schema.Choice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage:   a.usage,
	}
}
