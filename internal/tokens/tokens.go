// Package tokens provides a fast, dependency-free token estimate. It is not a
// real BPE tokenizer; it approximates ~4 characters per token, which is close
// enough for budgeting and metrics without pulling in model-specific vocab.
package tokens

import "frostgate/internal/schema"

// EstimateText approximates the token count of a string.
func EstimateText(s string) int {
	if s == "" {
		return 0
	}
	// ~4 chars/token is the well-known rough heuristic for English text.
	n := (len(s) + 3) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// EstimateMessages sums the estimate across messages, adding a small
// per-message overhead for role/formatting tokens (mirrors how chat models
// charge for message framing).
func EstimateMessages(msgs []schema.Message) int {
	total := 0
	for _, m := range msgs {
		total += 4 // per-message framing overhead
		if t, ok := m.TextContent(); ok {
			total += EstimateText(t)
		}
		total += len(m.ToolCalls) / 4
	}
	return total
}
