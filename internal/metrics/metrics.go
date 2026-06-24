// Package metrics tracks gateway counters and exposes them in Prometheus
// text exposition format without any external dependency.
package metrics

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// Metrics holds atomic counters.
type Metrics struct {
	Requests    atomic.Int64
	CacheHits   atomic.Int64
	Errors      atomic.Int64
	TokensSaved atomic.Int64

	mu          sync.Mutex
	byProvider  map[string]int64
}

// New builds a Metrics.
func New() *Metrics {
	return &Metrics{byProvider: make(map[string]int64)}
}

// ObserveProvider counts a request served by a provider.
func (m *Metrics) ObserveProvider(name string) {
	m.mu.Lock()
	m.byProvider[name]++
	m.mu.Unlock()
}

// Render returns Prometheus-format text.
func (m *Metrics) Render() string {
	var b []byte
	add := func(format string, args ...any) { b = append(b, []byte(fmt.Sprintf(format, args...))...) }

	add("# HELP frostgate_requests_total Total chat requests handled.\n")
	add("# TYPE frostgate_requests_total counter\n")
	add("frostgate_requests_total %d\n", m.Requests.Load())

	add("# HELP frostgate_cache_hits_total Total cache hits.\n")
	add("# TYPE frostgate_cache_hits_total counter\n")
	add("frostgate_cache_hits_total %d\n", m.CacheHits.Load())

	add("# HELP frostgate_errors_total Total failed requests.\n")
	add("# TYPE frostgate_errors_total counter\n")
	add("frostgate_errors_total %d\n", m.Errors.Load())

	add("# HELP frostgate_tokens_saved_total Prompt tokens saved by compression.\n")
	add("# TYPE frostgate_tokens_saved_total counter\n")
	add("frostgate_tokens_saved_total %d\n", m.TokensSaved.Load())

	add("# HELP frostgate_provider_requests_total Requests per upstream provider.\n")
	add("# TYPE frostgate_provider_requests_total counter\n")
	m.mu.Lock()
	names := make([]string, 0, len(m.byProvider))
	for n := range m.byProvider {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		add("frostgate_provider_requests_total{provider=%q} %d\n", n, m.byProvider[n])
	}
	m.mu.Unlock()
	return string(b)
}
