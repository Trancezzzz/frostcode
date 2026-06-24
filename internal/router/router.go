// Package router resolves a model alias to an ordered list of concrete
// (provider, model, apiKey) attempts, performing weighted key load balancing
// and automatic fallback across targets.
package router

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"strings"
	"sync"

	"frostgate/internal/config"
	"frostgate/internal/provider"
	"frostgate/internal/schema"
)

// Router holds the provider adapters and routing table.
type Router struct {
	providers map[string]provider.Provider
	models    map[string]config.ModelRoute
	keys      map[string][]config.Key // provider name -> key pool

	seed maphash.Seed
	mu   sync.Mutex
	rr   map[string]uint64 // round-robin counters per provider for tie-break
}

// New builds a Router from config and instantiated providers.
func New(cfg *config.Config, providers map[string]provider.Provider) *Router {
	keys := make(map[string][]config.Key)
	for name, p := range cfg.Providers {
		keys[name] = p.Keys
	}
	return &Router{
		providers: providers,
		models:    cfg.Models,
		keys:      keys,
		seed:      maphash.MakeSeed(),
		rr:        make(map[string]uint64),
	}
}

// Attempt is one concrete routing target with a chosen key.
type Attempt struct {
	Provider provider.Provider
	Model    string
	APIKey   string
}

// Resolve turns a requested model into an ordered list of attempts. It honors
// explicit model routes first; otherwise it parses "provider/model" syntax.
func (r *Router) Resolve(model string) ([]Attempt, error) {
	var targets []config.Target
	if route, ok := r.models[model]; ok {
		targets = route.Targets
	} else if p, m, ok := splitProviderModel(model); ok {
		targets = []config.Target{{Provider: p, Model: m}}
	} else {
		return nil, fmt.Errorf("unknown model %q: not in routing table and not in provider/model form", model)
	}

	var attempts []Attempt
	for _, t := range targets {
		p, ok := r.providers[t.Provider]
		if !ok {
			continue // skip targets whose provider isn't configured
		}
		key := r.pickKey(t.Provider)
		attempts = append(attempts, Attempt{Provider: p, Model: t.Model, APIKey: key})
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf("no usable targets for model %q", model)
	}
	return attempts, nil
}

// splitProviderModel parses "openai/gpt-4o-mini" -> ("openai","gpt-4o-mini").
func splitProviderModel(s string) (string, string, bool) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// pickKey selects an API key using weighted random selection, with a
// round-robin tie-break to spread load deterministically under equal weights.
func (r *Router) pickKey(providerName string) string {
	r.mu.Lock()
	pool := r.keys[providerName]
	if len(pool) == 0 {
		r.mu.Unlock()
		return ""
	}
	if len(pool) == 1 {
		v := pool[0].Value
		r.mu.Unlock()
		return v
	}
	total := 0
	for _, k := range pool {
		total += k.Weight
	}
	r.rr[providerName]++
	counter := r.rr[providerName]
	r.mu.Unlock()

	// Mix the counter into a hash for a fast, lock-light pseudo-random pick
	// weighted by key weight. ~constant time, no global RNG contention.
	var h maphash.Hash
	h.SetSeed(r.seed)
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(counter >> (i * 8))
	}
	_, _ = h.Write(buf[:])
	pick := int(h.Sum64() % uint64(total))
	for _, k := range pool {
		pick -= k.Weight
		if pick < 0 {
			return k.Value
		}
	}
	return pool[len(pool)-1].Value
}

// SetProviderKey replaces a provider's key pool with a single key at runtime.
// It is used by the CLI's /key command so the user can swap an API key without
// editing config and restarting. Returns false if the provider is not
// configured. The change is in-memory only (not persisted to the config file).
func (r *Router) SetProviderKey(providerName, key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[providerName]; !ok {
		return false
	}
	r.keys[providerName] = []config.Key{{Value: key, Weight: 1}}
	delete(r.rr, providerName)
	return true
}

// Providers returns the configured provider names (sorted-order not guaranteed).
func (r *Router) Providers() []string {
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	return names
}

// ErrAllFailed is returned when every fallback target fails.
var ErrAllFailed = errors.New("all routing targets failed")

// Chat runs attempts in order, returning the first success. Non-retryable
// errors (4xx other than 429) short-circuit. Returns the number of attempts
// made and the winning attempt for metadata.
func (r *Router) Chat(ctx context.Context, attempts []Attempt, req *schema.ChatRequest) (*schema.ChatResponse, Attempt, int, error) {
	var lastErr error
	for i, a := range attempts {
		resp, err := a.Provider.Chat(ctx, a.APIKey, a.Model, req)
		if err == nil {
			return resp, a, i + 1, nil
		}
		lastErr = err
		if !retryable(err) {
			return nil, a, i + 1, err
		}
	}
	return nil, Attempt{}, len(attempts), fmt.Errorf("%w: %v", ErrAllFailed, lastErr)
}

// Stream runs attempts in order for a streaming request. Because bytes may
// already be flushed to the client, fallback only applies to failures that
// happen before the first write (connection/4xx/5xx from the upstream call).
func (r *Router) Stream(ctx context.Context, attempts []Attempt, req *schema.ChatRequest, w io.Writer, flush func()) (Attempt, int, error) {
	var lastErr error
	for i, a := range attempts {
		err := a.Provider.Stream(ctx, a.APIKey, a.Model, req, w, flush)
		if err == nil {
			return a, i + 1, nil
		}
		lastErr = err
		if !retryable(err) {
			return a, i + 1, err
		}
	}
	return Attempt{}, len(attempts), fmt.Errorf("%w: %v", ErrAllFailed, lastErr)
}

func retryable(err error) bool {
	var he *provider.HTTPError
	if errors.As(err, &he) {
		return he.Retryable()
	}
	return true // network errors etc. are worth a fallback
}
