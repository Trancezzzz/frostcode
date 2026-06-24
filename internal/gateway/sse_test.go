package gateway

import (
	"strings"
	"testing"
)

// feed writes s to the assembler in small chunks to exercise partial-line
// buffering across Write calls.
func feed(a *sseAssembler, s string) {
	for i := 0; i < len(s); i += 7 {
		end := i + 7
		if end > len(s) {
			end = len(s)
		}
		_, _ = a.Write([]byte(s[i:end]))
	}
}

func TestSSEAssemblesText(t *testing.T) {
	var got strings.Builder
	a := newSSEAssembler(func(s string) { got.WriteString(s) }, nil)
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	feed(a, stream)

	if got.String() != "Hello world" {
		t.Fatalf("onText got %q", got.String())
	}
	resp := a.Response()
	txt, _ := resp.Choices[0].Message.TextContent()
	if txt != "Hello world" {
		t.Fatalf("assembled text %q", txt)
	}
	if resp.Usage.TotalTokens != 12 {
		t.Fatalf("usage not captured: %d", resp.Usage.TotalTokens)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish %q", resp.Choices[0].FinishReason)
	}
}

func TestSSEReassemblesSplitToolCall(t *testing.T) {
	a := newSSEAssembler(nil, nil)
	// A tool_call whose name and arguments arrive across several chunks.
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"a.txt\\\",\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"content\\\":\\\"hi\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
	feed(a, stream)

	resp := a.Response()
	calls := resp.Choices[0].Message.ParseToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Function.Name != "write_file" {
		t.Fatalf("bad tool call: %+v", calls[0])
	}
	want := `{"path":"a.txt","content":"hi"}`
	if calls[0].Function.Arguments != want {
		t.Fatalf("args reassembled as %q, want %q", calls[0].Function.Arguments, want)
	}
}
