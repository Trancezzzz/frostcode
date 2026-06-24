package compress

import (
	"context"
	"testing"

	"frostgate/internal/config"
	"frostgate/internal/schema"
)

func msg(role, text string) schema.Message {
	m := schema.Message{Role: role}
	m.SetText(text)
	return m
}

func TestWindowDropsMiddleKeepsRecentAndSystem(t *testing.T) {
	cfg := config.CompressionConfig{
		Enabled: true, Strategy: "window", MaxPromptTokens: 1, KeepRecentTurns: 2,
	}
	c := New(cfg, nil)
	req := &schema.ChatRequest{Messages: []schema.Message{
		msg("system", "you are helpful"),
		msg("user", "first question about cats"),
		msg("assistant", "answer about cats"),
		msg("user", "second question about dogs"),
		msg("assistant", "answer about dogs"),
		msg("user", "final question"),
	}}
	res := c.Apply(context.Background(), req)
	if res.Applied != "window" {
		t.Fatalf("expected window applied, got %q", res.Applied)
	}
	if res.TokensSaved <= 0 {
		t.Fatalf("expected tokens saved > 0, got %d", res.TokensSaved)
	}
	// System must survive.
	if req.Messages[0].Role != "system" {
		t.Fatalf("system message not preserved first")
	}
	// Last two non-system turns must survive verbatim.
	last, _ := req.Messages[len(req.Messages)-1].TextContent()
	if last != "final question" {
		t.Fatalf("recent turn not preserved, got %q", last)
	}
}

func TestTrimNormalizesWhitespace(t *testing.T) {
	cfg := config.CompressionConfig{Enabled: true, Strategy: "trim"}
	c := New(cfg, nil)
	req := &schema.ChatRequest{Messages: []schema.Message{
		msg("user", "hello     world\n\n\t  again"),
	}}
	c.Apply(context.Background(), req)
	got, _ := req.Messages[0].TextContent()
	if got != "hello world again" {
		t.Fatalf("trim failed: %q", got)
	}
}

func TestSummaryFallsBackToWindowWithoutSummarizer(t *testing.T) {
	cfg := config.CompressionConfig{
		Enabled: true, Strategy: "summary", MaxPromptTokens: 1, KeepRecentTurns: 1,
		SummaryModel: "", // no summarizer wired
	}
	c := New(cfg, nil)
	req := &schema.ChatRequest{Messages: []schema.Message{
		msg("user", "a long opening question with plenty of detail to drop"),
		msg("assistant", "a correspondingly long answer that takes up real tokens"),
		msg("user", "the final question we must keep"),
	}}
	res := c.Apply(context.Background(), req)
	if res.TokensSaved <= 0 {
		t.Fatalf("expected fallback window to save tokens")
	}
}

func TestDisabledIsNoOp(t *testing.T) {
	cfg := config.CompressionConfig{Enabled: false, Strategy: "window"}
	c := New(cfg, nil)
	req := &schema.ChatRequest{Messages: []schema.Message{msg("user", "x")}}
	res := c.Apply(context.Background(), req)
	if res.Applied != "" || res.TokensSaved != 0 {
		t.Fatalf("disabled should be no-op")
	}
}
