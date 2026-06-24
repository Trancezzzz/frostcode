// Command frostcode is a coding-agent CLI built on the Frostgate gateway. It
// gives a tool-using model (e.g. Kimi on NVIDIA NIM) the ability to read,
// write, edit, and delete files and run shell commands in your project — like
// Claude Code / opencode, routed through your own gateway.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"encoding/json"

	"frostgate/internal/agent"
	"frostgate/internal/config"
	"frostgate/internal/gateway"
	"frostgate/internal/mcp"
)

func main() {
	cfgPath := flag.String("config", "", "path to gateway config (default: $FROSTCODE_CONFIG, then standard locations)")
	model := flag.String("model", "", "model alias (default: $FROSTCODE_MODEL, then 'kimi')")
	root := flag.String("dir", ".", "project directory the agent may modify")
	auto := flag.Bool("auto", false, "auto-approve file/shell actions (use with care)")
	yolo := flag.Bool("yolo", false, "alias for -auto: run all actions without confirmation")
	plan := flag.Bool("plan", false, "start in plan mode (read-only)")
	resume := flag.String("resume", "", "resume a saved session by id")
	flag.Parse()

	resolved, err := resolveConfig(*cfgPath)
	if err != nil {
		fail("config: %v", err)
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		fail("config: %v", err)
	}

	// The gateway routes model calls; the agent owns MCP, so build the gateway
	// without its own MCP manager to avoid double-spawning stdio servers.
	gwCfg := *cfg
	gwCfg.MCP = config.MCPConfig{}
	gw, cleanup, err := gateway.FromConfig(&gwCfg)
	if err != nil {
		fail("gateway: %v", err)
	}
	defer cleanup()

	// Connect MCP servers and expose their tools to the agent.
	mcpMgr, mcpErrs := mcp.NewManager(cfg.MCP)
	for _, e := range mcpErrs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	defer mcpMgr.Close()

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fail("dir: %v", err)
	}

	models := modelAliases(cfg)
	chosen := *model
	if chosen == "" {
		chosen = os.Getenv("FROSTCODE_MODEL")
	}
	if chosen == "" {
		chosen = defaultModel(models)
	}

	repl := agent.NewREPL(agent.Options{
		Model:     chosen,
		Models:    models,
		Targets:   modelTargets(cfg),
		Caller:    gw, // *gateway.Gateway satisfies agent.Caller via ToolChat
		Root:      absRoot,
		SkillsDir:  filepath.Join(absRoot, ".frostcode", "skills"),
		Auto:       *auto || *yolo,
		Plan:       *plan,
		ResumeID:   *resume,
		ExtraTools: mcpAgentTools(mcpMgr),
		MCP:        mcpMgr,
	})
	repl.Run()
}

// mcpAgentTools converts an MCP manager's tool catalog into agent tools whose
// Run executes the call against the owning MCP server.
func mcpAgentTools(mgr *mcp.Manager) []agent.Tool {
	raw := mgr.ToolsJSON()
	if len(raw) == 0 {
		return nil
	}
	var specs []struct {
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil
	}
	out := make([]agent.Tool, 0, len(specs))
	for _, s := range specs {
		name := s.Function.Name
		out = append(out, agent.Tool{
			Name:        name,
			Description: "[MCP] " + s.Function.Description,
			Schema:      s.Function.Parameters,
			Destructive: true, // MCP tools may have side effects; require approval
			Run: func(a map[string]any) (string, error) {
				b, _ := json.Marshal(a)
				return mgr.Call(name, b)
			},
		})
	}
	return out
}

// modelTargets maps each alias to its first "provider/model" target for display.
func modelTargets(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Models))
	for alias, route := range cfg.Models {
		if len(route.Targets) > 0 {
			out[alias] = route.Targets[0].Provider + "/" + route.Targets[0].Model
		}
	}
	return out
}

// resolveConfig finds the gateway config: explicit flag, then $FROSTCODE_CONFIG,
// then a config.json next to the executable, then ~/.frostcode/config.json,
// then ./config.json. This lets the installed `frostcode` command work from any
// project directory while still finding the gateway config (with the NIM key).
func resolveConfig(flagPath string) (string, error) {
	var candidates []string
	if flagPath != "" {
		candidates = append(candidates, flagPath)
	}
	if env := os.Getenv("FROSTCODE_CONFIG"); env != "" {
		candidates = append(candidates, env)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".frostcode", "config.json"))
	}
	candidates = append(candidates, "config.json")

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no config found; set $FROSTCODE_CONFIG or pass -config (looked in: %s)",
		strings.Join(candidates, ", "))
}

// modelAliases returns the configured model aliases, sorted.
func modelAliases(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Models))
	for k := range cfg.Models {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// defaultModel prefers a "kimi" alias if present, else the first alias.
func defaultModel(models []string) string {
	for _, m := range models {
		if m == "kimi" {
			return m
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return "kimi"
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
