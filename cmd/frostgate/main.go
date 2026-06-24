// Command frostgate is a high-performance, OpenAI-compatible AI gateway with
// automatic fallbacks, weighted key load balancing, response caching, and a
// token-saving context-compression mode.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"frostgate/internal/config"
	"frostgate/internal/gateway"
	"frostgate/internal/mcp"
	"frostgate/internal/provider"
	"frostgate/internal/server"
	"frostgate/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	providers, err := buildProviders(cfg)
	if err != nil {
		log.Fatalf("providers: %v", err)
	}

	// Persistence store (file-backed when enabled; in-memory otherwise).
	var st store.Store
	if cfg.Persistence.Enabled {
		st = store.NewFile(cfg.Persistence.Path)
	} else {
		st = store.NewMemory()
	}

	// MCP manager connects to tool servers (best-effort; bad servers logged).
	mcpMgr, mcpErrs := mcp.NewManager(cfg.MCP)
	for _, e := range mcpErrs {
		log.Printf("warning: %v", e)
	}
	defer mcpMgr.Close()

	gw := gateway.New(cfg, providers, mcpMgr, st)
	srv := server.New(gw)

	// Periodically persist governance spend so budgets survive restarts.
	if cfg.Persistence.Enabled {
		startFlushLoop(gw, time.Duration(cfg.Persistence.FlushSeconds)*time.Second)
	}

	fmt.Printf("frostgate node %q listening on %s\n", cfg.Cluster.NodeID, cfg.Listen)
	fmt.Printf("  dashboard: http://localhost%s/\n", dashboardHost(cfg.Listen))
	fmt.Printf("  providers: %v\n", keys(providers))
	fmt.Printf("  cache=%v compression=%s/%s governance=%v mcp=%v(%d tools) cluster=%v\n",
		cfg.Cache.Enabled, boolToOnOff(cfg.Compression.Enabled), cfg.Compression.Strategy,
		cfg.Governance.Enabled, cfg.MCP.Enabled, len(mcpMgr.ToolNames()), cfg.Cluster.Enabled)

	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// startFlushLoop periodically persists governance spend in the background.
func startFlushLoop(gw *gateway.Gateway, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Second
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			if err := gw.Governor.Flush(); err != nil {
				log.Printf("warning: state flush: %v", err)
			}
		}
	}()
}

// buildProviders instantiates an adapter per configured provider.
func buildProviders(cfg *config.Config) (map[string]provider.Provider, error) {
	out := make(map[string]provider.Provider)
	for name, p := range cfg.Providers {
		switch p.Kind {
		case "openai":
			out[name] = provider.NewOpenAI(name, p.BaseURL)
		case "anthropic":
			out[name] = provider.NewAnthropic(name, p.BaseURL)
		case "mock":
			out[name] = provider.NewMock(name)
		default:
			return nil, fmt.Errorf("provider %q: unknown kind %q", name, p.Kind)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no providers configured")
	}
	return out, nil
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func boolToOnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// dashboardHost turns a bind address like ":8080" into a host clients can
// open, defaulting the empty host to the port as-is.
func dashboardHost(listen string) string {
	return listen
}

var _ = os.Getenv // keep os imported for future use
