// Package compress implements the token-saving middleware ("save token mode").
// It shrinks a chat request's prompt before it hits an upstream provider,
// trading away old context to fit a token budget. Four strategies:
//
//	off     - no-op.
//	trim    - normalize whitespace in every message (cheap, near-lossless).
//	window  - always keep system messages + the last N turns; drop the
//	          middle when over budget.
//	summary - like window, but the dropped middle is replaced by a single
//	          system note summarized by a cheap model, so old context is
//	          retained in compressed form instead of being discarded.
package compress

import (
	"context"
	"strings"

	"frostgate/internal/config"
	"frostgate/internal/schema"
	"frostgate/internal/tokens"
)

// Summarizer produces a short summary of dropped history. It is injected so
// this package doesn't depend on the router directly (avoids an import cycle).
type Summarizer func(ctx context.Context, model string, history []schema.Message) (string, error)

// Compressor applies a configured strategy to requests.
type Compressor struct {
	cfg        config.CompressionConfig
	summarize  Summarizer
}

// New builds a Compressor. summarize may be nil if the "summary" strategy is
// unused; it then degrades to "window".
func New(cfg config.CompressionConfig, summarize Summarizer) *Compressor {
	return &Compressor{cfg: cfg, summarize: summarize}
}

// Result reports what compression did, for response metadata and metrics.
type Result struct {
	Applied     string // strategy actually applied ("" if none)
	BeforeTok   int
	AfterTok    int
	TokensSaved int
}

// Apply compresses req in place (mutates req.Messages) and returns a Result.
// It is a no-op when disabled, when the strategy is "off", or — for the
// window/summary strategies — when the prompt is already under budget.
func (c *Compressor) Apply(ctx context.Context, req *schema.ChatRequest) Result {
	res := Result{}
	if !c.cfg.Enabled || c.cfg.Strategy == "off" {
		return res
	}
	before := tokens.EstimateMessages(req.Messages)
	res.BeforeTok = before

	switch c.cfg.Strategy {
	case "trim":
		c.trim(req)
	case "window":
		if c.overBudget(before) {
			c.window(req)
		}
	case "summary":
		if c.overBudget(before) {
			c.summary(ctx, req)
		}
	default:
		// Unknown strategy: behave as window to stay safe.
		if c.overBudget(before) {
			c.window(req)
		}
	}

	after := tokens.EstimateMessages(req.Messages)
	res.AfterTok = after
	if after < before {
		res.Applied = c.cfg.Strategy
		res.TokensSaved = before - after
	}
	return res
}

// overBudget reports whether the prompt exceeds the configured trigger. A
// MaxPromptTokens of 0 means "always compress" for window/summary.
func (c *Compressor) overBudget(n int) bool {
	if c.cfg.MaxPromptTokens <= 0 {
		return true
	}
	return n > c.cfg.MaxPromptTokens
}

// trim collapses runs of whitespace in each message's text content. This is
// the cheapest saving and rarely changes meaning.
func (c *Compressor) trim(req *schema.ChatRequest) {
	for i := range req.Messages {
		if t, ok := req.Messages[i].TextContent(); ok {
			cleaned := strings.Join(strings.Fields(t), " ")
			if cleaned != t {
				req.Messages[i].SetText(cleaned)
			}
		}
	}
}

// split separates messages into system messages (always kept up front), the
// older "middle" eligible for dropping, and the recent tail kept verbatim.
func (c *Compressor) split(msgs []schema.Message) (system, middle, recent []schema.Message) {
	var nonSystem []schema.Message
	for _, m := range msgs {
		if m.Role == "system" {
			system = append(system, m)
		} else {
			nonSystem = append(nonSystem, m)
		}
	}
	keep := c.cfg.KeepRecentTurns
	if keep >= len(nonSystem) {
		return system, nil, nonSystem
	}
	cut := len(nonSystem) - keep
	return system, nonSystem[:cut], nonSystem[cut:]
}

// window keeps system + recent, dropping the middle entirely.
func (c *Compressor) window(req *schema.ChatRequest) {
	system, middle, recent := c.split(req.Messages)
	if len(middle) == 0 {
		return // nothing to drop
	}
	out := make([]schema.Message, 0, len(system)+len(recent)+1)
	out = append(out, system...)
	// Leave a breadcrumb so the model knows history was elided.
	note := schema.Message{Role: "system"}
	note.SetText("[frostgate: omitted " + itoa(len(middle)) + " earlier message(s) to save tokens]")
	out = append(out, note)
	out = append(out, recent...)
	req.Messages = out
}

// summary replaces the dropped middle with a model-generated summary. Falls
// back to window behavior if no summarizer is wired or summarization fails.
func (c *Compressor) summary(ctx context.Context, req *schema.ChatRequest) {
	system, middle, recent := c.split(req.Messages)
	if len(middle) == 0 {
		return
	}
	if c.summarize == nil || c.cfg.SummaryModel == "" {
		c.window(req)
		return
	}
	sum, err := c.summarize(ctx, c.cfg.SummaryModel, middle)
	if err != nil || strings.TrimSpace(sum) == "" {
		c.window(req) // graceful degradation
		return
	}
	out := make([]schema.Message, 0, len(system)+len(recent)+1)
	out = append(out, system...)
	note := schema.Message{Role: "system"}
	note.SetText("Summary of earlier conversation (compressed to save tokens):\n" + sum)
	out = append(out, note)
	out = append(out, recent...)
	req.Messages = out
}

// itoa is a tiny int->string to avoid importing strconv for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
