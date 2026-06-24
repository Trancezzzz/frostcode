package gateway

import (
	"context"
	"testing"

	"frostgate/internal/config"
	"frostgate/internal/provider"
	"frostgate/internal/schema"
)

func newTestGateway() *Gateway {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"local": {Kind: "mock", Keys: []config.Key{{Value: "none", Weight: 1}}},
		},
		Models: map[string]config.ModelRoute{
			"demo": {Targets: []config.Target{{Provider: "local", Model: "demo-1"}}},
		},
		Cache:       config.CacheConfig{Enabled: true, TTLSeconds: 60, MaxEntries: 100},
		Compression: config.CompressionConfig{Enabled: false},
	}
	providers := map[string]provider.Provider{"local": provider.NewMock("local")}
	return New(cfg, providers, nil, nil)
}

func userReq(model, text string) *schema.ChatRequest {
	m := schema.Message{Role: "user"}
	m.SetText(text)
	return &schema.ChatRequest{Model: model, Messages: []schema.Message{m}}
}

func TestCompleteRoutesToMock(t *testing.T) {
	g := newTestGateway()
	resp, err := g.Complete(context.Background(), userReq("demo", "hello"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Gateway == nil || resp.Gateway.Provider != "local" {
		t.Fatalf("expected provider=local, got %+v", resp.Gateway)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("no choices returned")
	}
}

func TestCacheHitOnSecondCall(t *testing.T) {
	g := newTestGateway()
	ctx := context.Background()
	_, err := g.Complete(ctx, userReq("demo", "same question"))
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	resp, err := g.Complete(ctx, userReq("demo", "same question"))
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if resp.Gateway == nil || !resp.Gateway.CacheHit {
		t.Fatalf("expected cache hit on second identical call")
	}
	if g.Metrics.CacheHits.Load() != 1 {
		t.Fatalf("expected 1 cache hit metric, got %d", g.Metrics.CacheHits.Load())
	}
}

func TestProviderPrefixRouting(t *testing.T) {
	g := newTestGateway()
	// "local/anything" should route via provider-prefix parsing even though
	// it isn't in the models table.
	resp, err := g.Complete(context.Background(), userReq("local/anymodel", "hi"))
	if err != nil {
		t.Fatalf("prefix routing: %v", err)
	}
	if resp.Gateway.ResolvedModel != "anymodel" {
		t.Fatalf("expected resolved model 'anymodel', got %q", resp.Gateway.ResolvedModel)
	}
}

func TestUnknownModelErrors(t *testing.T) {
	g := newTestGateway()
	_, err := g.Complete(context.Background(), userReq("nonexistent", "hi"))
	if err == nil {
		t.Fatalf("expected error for unknown model")
	}
}
