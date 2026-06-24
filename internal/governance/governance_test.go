package governance

import (
	"testing"
	"time"

	"frostgate/internal/config"
)

func newGov() (*Governor, *time.Time) {
	clock := time.Unix(1000, 0)
	cfg := config.GovernanceConfig{
		Enabled: true,
		VirtualKeys: []config.VirtualKey{
			{Key: "vk-rate", Name: "rate", RPM: 2},
			{Key: "vk-budget", Name: "budget", MaxTokens: 100},
			{Key: "vk-model", Name: "model", AllowedModels: []string{"allowed"}},
		},
	}
	g := New(cfg, nil, nil)
	g.now = func() time.Time { return clock }
	return g, &clock
}

func TestDisabledAllowsEverything(t *testing.T) {
	g := New(config.GovernanceConfig{Enabled: false}, nil, nil)
	if r := g.Check("", "anything"); r.Decision != Allow {
		t.Fatalf("disabled governance should allow, got %v", r.Decision)
	}
}

func TestUnknownKeyDenied(t *testing.T) {
	g, _ := newGov()
	if r := g.Check("nope", "m"); r.Decision != DenyUnauthorized {
		t.Fatalf("expected unauthorized, got %v", r.Decision)
	}
}

func TestRateLimitTokenBucket(t *testing.T) {
	g, clock := newGov()
	// RPM=2 => burst of 2 allowed immediately, 3rd denied.
	if r := g.Check("vk-rate", "m"); r.Decision != Allow {
		t.Fatalf("call 1 should allow, got %v", r.Decision)
	}
	if r := g.Check("vk-rate", "m"); r.Decision != Allow {
		t.Fatalf("call 2 should allow, got %v", r.Decision)
	}
	if r := g.Check("vk-rate", "m"); r.Decision != DenyRateLimited {
		t.Fatalf("call 3 should be rate limited, got %v", r.Decision)
	}
	// Advance 30s => refill 1 token (2 per 60s).
	*clock = clock.Add(30 * time.Second)
	if r := g.Check("vk-rate", "m"); r.Decision != Allow {
		t.Fatalf("after refill should allow, got %v", r.Decision)
	}
}

func TestBudgetExhaustion(t *testing.T) {
	g, _ := newGov()
	if r := g.Check("vk-budget", "m"); r.Decision != Allow {
		t.Fatalf("first call under budget should allow, got %v", r.Decision)
	}
	g.RecordSpend("vk-budget", 100) // hits the cap
	if r := g.Check("vk-budget", "m"); r.Decision != DenyBudget {
		t.Fatalf("expected budget denial after spend, got %v", r.Decision)
	}
}

func TestModelAllowList(t *testing.T) {
	g, _ := newGov()
	if r := g.Check("vk-model", "blocked"); r.Decision != DenyModel {
		t.Fatalf("expected model denial, got %v", r.Decision)
	}
	if r := g.Check("vk-model", "allowed"); r.Decision != Allow {
		t.Fatalf("allowed model should pass, got %v", r.Decision)
	}
}

func TestSnapshotReportsUsage(t *testing.T) {
	g, _ := newGov()
	g.Check("vk-budget", "m")
	g.RecordSpend("vk-budget", 42)
	for _, u := range g.Snapshot() {
		if u.Name == "budget" {
			if u.SpentTokens != 42 || u.Requests != 1 {
				t.Fatalf("snapshot wrong: %+v", u)
			}
			return
		}
	}
	t.Fatalf("budget key not found in snapshot")
}
