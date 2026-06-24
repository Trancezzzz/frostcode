// Package governance implements authentication (static virtual keys and OIDC
// JWT bearer tokens), per-identity rate limiting (token bucket), cumulative
// token budgets, and model allow-lists. Spend can be persisted through a Store
// so budgets survive restarts and can be shared across co-located nodes.
package governance

import (
	"strings"
	"sync"
	"time"

	"frostgate/internal/config"
	"frostgate/internal/oidc"
	"frostgate/internal/store"
)

// Decision is the outcome of a governance check.
type Decision int

const (
	Allow            Decision = iota
	DenyUnauthorized          // unknown/missing key or invalid token
	DenyRateLimited           // RPM exceeded
	DenyBudget                // token budget exhausted
	DenyModel                 // model not in identity's allow-list
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case DenyUnauthorized:
		return "unauthorized"
	case DenyRateLimited:
		return "rate_limited"
	case DenyBudget:
		return "budget_exceeded"
	case DenyModel:
		return "model_not_allowed"
	}
	return "unknown"
}

// keyState holds the mutable per-identity counters.
type keyState struct {
	cfg config.VirtualKey

	mu          sync.Mutex
	tokens      float64 // token-bucket level
	lastFill    time.Time
	spentTokens int64
	requests    int64
}

// Governor enforces governance across static keys and OIDC identities.
type Governor struct {
	enabled  bool
	now      func() time.Time
	verifier *oidc.Verifier
	oidcCfg  config.OIDCConfig
	st       store.Store

	mu          sync.RWMutex
	keys        map[string]*keyState // virtual-key secret -> state
	dyn         map[string]*keyState // OIDC subject -> state
	loadedSpend map[string]int64     // name -> spend, from the store at boot
}

// New builds a Governor. verifier may be nil (no OIDC). st may be nil (uses an
// in-memory store). It loads any persisted spend so budgets resume.
func New(cfg config.GovernanceConfig, verifier *oidc.Verifier, st store.Store) *Governor {
	if st == nil {
		st = store.NewMemory()
	}
	loaded, _ := st.Load()
	g := &Governor{
		enabled:     cfg.Enabled,
		now:         time.Now,
		verifier:    verifier,
		oidcCfg:     cfg.OIDC,
		st:          st,
		keys:        make(map[string]*keyState),
		dyn:         make(map[string]*keyState),
		loadedSpend: loaded.Spend,
	}
	for _, vk := range cfg.VirtualKeys {
		g.keys[vk.Key] = &keyState{
			cfg: vk, tokens: float64(vk.RPM), lastFill: g.now(),
			spentTokens: g.loadedSpend[vk.Name],
		}
	}
	return g
}

// Enabled reports whether auth is enforced.
func (g *Governor) Enabled() bool { return g.enabled }

// CheckResult carries the decision plus the resolved identity and a billing id
// the caller passes back to RecordSpend.
type CheckResult struct {
	Decision  Decision
	KeyName   string
	BillingID string
}

// Authorize authenticates a bearer token (virtual key or OIDC JWT) and applies
// rate/budget/model checks. token is the raw value after "Bearer ".
func (g *Governor) Authorize(token, model string) CheckResult {
	if !g.enabled {
		return CheckResult{Decision: Allow, KeyName: "anonymous"}
	}
	// OIDC JWT path: a compact JWT has exactly two dots.
	if g.verifier != nil && strings.Count(token, ".") == 2 {
		claims, err := g.verifier.Verify(token)
		if err != nil {
			return CheckResult{Decision: DenyUnauthorized}
		}
		st := g.oidcState(claims.Sub)
		res := g.checkState(st, model)
		res.BillingID = "oidc:" + claims.Sub
		return res
	}
	// Static virtual-key path.
	g.mu.RLock()
	st, ok := g.keys[token]
	g.mu.RUnlock()
	if !ok || token == "" {
		return CheckResult{Decision: DenyUnauthorized}
	}
	res := g.checkState(st, model)
	res.BillingID = token
	return res
}

// Check is a convenience wrapper for the virtual-key path (used in tests).
func (g *Governor) Check(secret, model string) CheckResult {
	return g.Authorize(secret, model)
}

// oidcState returns (creating if needed) the state for an OIDC subject, seeded
// with the configured default limits and any persisted spend.
func (g *Governor) oidcState(sub string) *keyState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st, ok := g.dyn[sub]; ok {
		return st
	}
	name := "oidc:" + sub
	st := &keyState{
		cfg: config.VirtualKey{
			Name: name, RPM: g.oidcCfg.DefaultRPM, MaxTokens: g.oidcCfg.DefaultMaxTokens,
		},
		tokens: float64(g.oidcCfg.DefaultRPM), lastFill: g.now(),
		spentTokens: g.loadedSpend[name],
	}
	g.dyn[sub] = st
	return st
}

// checkState applies model/budget/rate checks to one identity.
func (g *Governor) checkState(st *keyState, model string) CheckResult {
	st.mu.Lock()
	defer st.mu.Unlock()

	if len(st.cfg.AllowedModels) > 0 && !contains(st.cfg.AllowedModels, model) {
		return CheckResult{Decision: DenyModel, KeyName: st.cfg.Name}
	}
	if st.cfg.MaxTokens > 0 && st.spentTokens >= st.cfg.MaxTokens {
		return CheckResult{Decision: DenyBudget, KeyName: st.cfg.Name}
	}
	if st.cfg.RPM > 0 {
		now := g.now()
		elapsed := now.Sub(st.lastFill).Seconds()
		if elapsed < 0 {
			elapsed = 0
		}
		st.lastFill = now
		st.tokens += elapsed * (float64(st.cfg.RPM) / 60.0)
		if st.tokens > float64(st.cfg.RPM) {
			st.tokens = float64(st.cfg.RPM)
		}
		if st.tokens < 1 {
			return CheckResult{Decision: DenyRateLimited, KeyName: st.cfg.Name}
		}
		st.tokens--
	}
	st.requests++
	return CheckResult{Decision: Allow, KeyName: st.cfg.Name}
}

// RecordSpend adds consumed tokens to an identity's total, keyed by the
// BillingID returned from Authorize.
func (g *Governor) RecordSpend(billingID string, totalTokens int) {
	if !g.enabled || totalTokens <= 0 || billingID == "" {
		return
	}
	var st *keyState
	g.mu.RLock()
	if strings.HasPrefix(billingID, "oidc:") {
		st = g.dyn[strings.TrimPrefix(billingID, "oidc:")]
	} else {
		st = g.keys[billingID]
	}
	g.mu.RUnlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	st.spentTokens += int64(totalTokens)
	st.mu.Unlock()
}

// Flush writes current spend to the store. Safe to call periodically.
func (g *Governor) Flush() error {
	snap := store.Snapshot{Spend: map[string]int64{}}
	g.mu.RLock()
	for _, st := range g.keys {
		st.mu.Lock()
		snap.Spend[st.cfg.Name] = st.spentTokens
		st.mu.Unlock()
	}
	for _, st := range g.dyn {
		st.mu.Lock()
		snap.Spend[st.cfg.Name] = st.spentTokens
		st.mu.Unlock()
	}
	g.mu.RUnlock()
	return g.st.Save(snap)
}

// Usage is a snapshot of one identity's consumption for dashboards.
type Usage struct {
	Name        string `json:"name"`
	Requests    int64  `json:"requests"`
	SpentTokens int64  `json:"spent_tokens"`
	MaxTokens   int64  `json:"max_tokens"`
	RPM         int    `json:"rpm"`
}

// Snapshot returns per-identity usage across static and OIDC identities.
func (g *Governor) Snapshot() []Usage {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Usage, 0, len(g.keys)+len(g.dyn))
	collect := func(m map[string]*keyState) {
		for _, st := range m {
			st.mu.Lock()
			out = append(out, Usage{
				Name: st.cfg.Name, Requests: st.requests, SpentTokens: st.spentTokens,
				MaxTokens: st.cfg.MaxTokens, RPM: st.cfg.RPM,
			})
			st.mu.Unlock()
		}
	}
	collect(g.keys)
	collect(g.dyn)
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
