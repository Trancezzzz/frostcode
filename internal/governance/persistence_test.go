package governance

import (
	"testing"

	"frostgate/internal/config"
	"frostgate/internal/store"
)

// TestSpendPersistsAcrossRestart proves a budget survives a process restart:
// flush to a store, then build a fresh Governor from the same store and verify
// the prior spend counts against the budget.
func TestSpendPersistsAcrossRestart(t *testing.T) {
	st := store.NewMemory()
	cfg := config.GovernanceConfig{
		Enabled: true,
		VirtualKeys: []config.VirtualKey{
			{Key: "vk", Name: "team", MaxTokens: 100},
		},
	}

	g1 := New(cfg, nil, st)
	if r := g1.Authorize("vk", "m"); r.Decision != Allow {
		t.Fatalf("first authorize should allow, got %v", r.Decision)
	}
	g1.RecordSpend("vk", 100) // exhaust budget
	if err := g1.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// "Restart": new Governor, same store.
	g2 := New(cfg, nil, st)
	if r := g2.Authorize("vk", "m"); r.Decision != DenyBudget {
		t.Fatalf("after restart budget should be exhausted, got %v", r.Decision)
	}
}
