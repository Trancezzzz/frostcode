package server

import (
	"net/http"
)

// handleStatus returns a JSON snapshot used by the dashboard: counters,
// configured providers/models, and per-virtual-key usage.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.gw.Config
	m := s.gw.Metrics

	providers := make([]map[string]any, 0, len(cfg.Providers))
	for name, p := range cfg.Providers {
		providers = append(providers, map[string]any{
			"name": name, "kind": p.Kind, "keys": len(p.Keys),
		})
	}
	models := make([]map[string]any, 0, len(cfg.Models))
	for alias, route := range cfg.Models {
		chain := make([]string, 0, len(route.Targets))
		for _, t := range route.Targets {
			chain = append(chain, t.Provider+"/"+t.Model)
		}
		models = append(models, map[string]any{"alias": alias, "chain": chain})
	}

	var mcpTools []string
	if s.gw.MCP != nil {
		mcpTools = s.gw.MCP.ToolNames()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service": "frostgate",
		"node":    s.gw.NodeID,
		"cluster": map[string]any{
			"enabled": cfg.Cluster.Enabled,
			"node_id": cfg.Cluster.NodeID,
		},
		"mcp": map[string]any{
			"enabled": cfg.MCP.Enabled,
			"tools":   mcpTools,
		},
		"persistence": map[string]any{
			"enabled": cfg.Persistence.Enabled,
			"path":    cfg.Persistence.Path,
		},
		"counters": map[string]any{
			"requests":     m.Requests.Load(),
			"cache_hits":   m.CacheHits.Load(),
			"errors":       m.Errors.Load(),
			"tokens_saved": m.TokensSaved.Load(),
		},
		"governance": map[string]any{
			"enabled": s.gw.Governor.Enabled(),
			"keys":    s.gw.Governor.Snapshot(),
		},
		"compression": map[string]any{
			"enabled":  cfg.Compression.Enabled,
			"strategy": cfg.Compression.Strategy,
		},
		"cache":     map[string]any{"enabled": cfg.Cache.Enabled},
		"providers": providers,
		"models":    models,
	})
}

// handleCluster reports this node's cluster identity and how it shares state.
// Nodes pointed at the same persistence store share governance budgets.
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	cfg := s.gw.Config
	backend := "memory (node-local)"
	if cfg.Persistence.Enabled {
		backend = "file: " + cfg.Persistence.Path
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":       cfg.Cluster.NodeID,
		"cluster":       cfg.Cluster.Enabled,
		"shared_state":  backend,
		"flush_seconds": cfg.Persistence.FlushSeconds,
	})
}

// handleDashboard serves the single-page dashboard. It polls /status.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}
