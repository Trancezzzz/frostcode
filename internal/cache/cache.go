// Package cache provides response caching with two modes: exact match on a
// normalized hash of the whole request, and "semantic-lite" match based on
// token-overlap cosine similarity of the final user message. It is in-memory,
// TTL-bounded, and safe for concurrent use.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"

	"frostgate/internal/schema"
)

// Cache is a TTL-bounded response cache.
type Cache struct {
	enabled   bool
	ttl       time.Duration
	maxItems  int
	threshold float64

	mu    sync.RWMutex
	items map[string]*entry
	order []string // insertion order for cheap FIFO eviction
}

type entry struct {
	resp      *schema.ChatResponse
	expires   time.Time
	model     string
	vec       map[string]float64 // term-frequency vector of final user msg
	vecNorm   float64
}

// New builds a cache. ttlSeconds, maxItems, and threshold come from config.
func New(enabled bool, ttlSeconds, maxItems int, threshold float64) *Cache {
	return &Cache{
		enabled:   enabled,
		ttl:       time.Duration(ttlSeconds) * time.Second,
		maxItems:  maxItems,
		threshold: threshold,
		items:     make(map[string]*entry),
	}
}

// keyFor builds a stable hash of the routing-relevant request fields.
func keyFor(model string, req *schema.ChatRequest) string {
	h := sha256.New()
	h.Write([]byte(model))
	for _, m := range req.Messages {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		if t, ok := m.TextContent(); ok {
			h.Write([]byte(normalize(t)))
		}
		h.Write([]byte{0})
	}
	if req.Temperature != nil {
		b, _ := json.Marshal(*req.Temperature)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Get looks up a cached response. It first tries exact match; if a similarity
// threshold is configured it then scans for a semantically close entry with
// the same model. Returns (resp, true) on hit.
func (c *Cache) Get(model string, req *schema.ChatRequest) (*schema.ChatResponse, bool) {
	if !c.enabled || req.Stream {
		return nil, false // never serve streaming from cache
	}
	now := time.Now()
	key := keyFor(model, req)

	c.mu.RLock()
	defer c.mu.RUnlock()

	if e, ok := c.items[key]; ok && now.Before(e.expires) {
		return clone(e.resp, true), true
	}
	if c.threshold <= 0 {
		return nil, false
	}
	// Semantic-lite scan over the final user message vector.
	qVec, qNorm := vectorize(finalUserText(req))
	if qNorm == 0 {
		return nil, false
	}
	var best *entry
	bestSim := c.threshold
	for _, e := range c.items {
		if e.model != model || now.After(e.expires) || e.vecNorm == 0 {
			continue
		}
		sim := cosine(qVec, qNorm, e.vec, e.vecNorm)
		if sim >= bestSim {
			bestSim = sim
			best = e
		}
	}
	if best != nil {
		return clone(best.resp, true), true
	}
	return nil, false
}

// Put stores a response.
func (c *Cache) Put(model string, req *schema.ChatRequest, resp *schema.ChatResponse) {
	if !c.enabled || req.Stream || resp == nil {
		return
	}
	key := keyFor(model, req)
	vec, norm := vectorize(finalUserText(req))
	e := &entry{
		resp:    clone(resp, false),
		expires: time.Now().Add(c.ttl),
		model:   model,
		vec:     vec,
		vecNorm: norm,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists {
		c.order = append(c.order, key)
	}
	c.items[key] = e
	// FIFO eviction past the cap.
	for len(c.items) > c.maxItems && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}
}

// --- text similarity helpers ---

func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func finalUserText(req *schema.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			t, _ := req.Messages[i].TextContent()
			return t
		}
	}
	return ""
}

// vectorize builds a term-frequency vector and its L2 norm.
func vectorize(s string) (map[string]float64, float64) {
	vec := make(map[string]float64)
	for _, tok := range strings.Fields(normalize(s)) {
		vec[tok]++
	}
	var sum float64
	for _, v := range vec {
		sum += v * v
	}
	return vec, math.Sqrt(sum)
}

// cosine computes cosine similarity given precomputed norms.
func cosine(a map[string]float64, aNorm float64, b map[string]float64, bNorm float64) float64 {
	if aNorm == 0 || bNorm == 0 {
		return 0
	}
	// Iterate the smaller map for speed.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	var dot float64
	for k, v := range small {
		if w, ok := large[k]; ok {
			dot += v * w
		}
	}
	return dot / (aNorm * bNorm)
}

// clone deep-copies a response and stamps the cache_hit flag when serving.
func clone(r *schema.ChatResponse, hit bool) *schema.ChatResponse {
	if r == nil {
		return nil
	}
	b, _ := json.Marshal(r)
	var out schema.ChatResponse
	_ = json.Unmarshal(b, &out)
	if hit {
		if out.Gateway == nil {
			out.Gateway = &schema.GatewayMeta{}
		}
		out.Gateway.CacheHit = true
	}
	return &out
}
