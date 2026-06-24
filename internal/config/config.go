// Package config loads gateway configuration from a JSON file with
// environment-variable substitution for secrets (e.g. "env:OPENAI_API_KEY").
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the top-level gateway configuration.
type Config struct {
	// Listen is the HTTP bind address, e.g. ":8080".
	Listen string `json:"listen"`

	// Providers maps a provider name ("openai", "anthropic", ...) to its
	// connection settings.
	Providers map[string]Provider `json:"providers"`

	// Models maps a public model alias to a routing target with an ordered
	// fallback chain. The first entry is preferred; the rest are tried on
	// failure. If a requested model isn't listed here, the gateway falls
	// back to provider-prefix routing ("openai/gpt-4o-mini").
	Models map[string]ModelRoute `json:"models"`

	Cache       CacheConfig       `json:"cache"`
	Compression CompressionConfig `json:"compression"`
	Governance  GovernanceConfig  `json:"governance"`
	MCP         MCPConfig         `json:"mcp"`
	Persistence PersistenceConfig `json:"persistence"`
	Cluster     ClusterConfig     `json:"cluster"`
}

// MCPConfig wires the gateway to Model Context Protocol servers so models can
// call external tools (filesystem, web search, databases, ...). The gateway
// acts as MCP client + tool executor, running an agentic loop on the model's
// behalf.
type MCPConfig struct {
	Enabled bool `json:"enabled"`
	// MaxToolIterations caps how many tool-call rounds one request may run
	// before the gateway returns the latest model output (loop guard).
	MaxToolIterations int `json:"max_tool_iterations"`
	// Servers is the set of MCP servers to connect to at startup.
	Servers []MCPServer `json:"servers"`
}

// MCPServer describes one MCP server connection.
type MCPServer struct {
	Name string `json:"name"`
	// Transport is "http" (JSON-RPC over POST) or "stdio" (subprocess).
	Transport string `json:"transport"`
	// URL is used when Transport is "http".
	URL string `json:"url,omitempty"`
	// Command/Args launch the server when Transport is "stdio".
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}

// PersistenceConfig controls durable state (governance spend, etc.).
type PersistenceConfig struct {
	Enabled bool `json:"enabled"`
	// Path is the JSON state file. Multiple co-located nodes may share it.
	Path string `json:"path"`
	// FlushSeconds is how often state is written back.
	FlushSeconds int `json:"flush_seconds"`
}

// ClusterConfig enables multi-node awareness. Nodes that share a Persistence
// store (shared filesystem path today; Redis in a production deployment)
// share governance budgets.
type ClusterConfig struct {
	Enabled bool `json:"enabled"`
	// NodeID labels this node in /cluster and logs. Defaults to the hostname.
	NodeID string `json:"node_id"`
}

// GovernanceConfig controls authentication, rate limiting, and budgets.
type GovernanceConfig struct {
	// Enabled turns on virtual-key auth. When false the gateway is open
	// (no Authorization required) — convenient for local/demo use.
	Enabled bool `json:"enabled"`
	// VirtualKeys is the set of issued client credentials.
	VirtualKeys []VirtualKey `json:"virtual_keys"`
	// OIDC optionally accepts RS256 JWT bearer tokens validated against a
	// JWKS endpoint, in addition to static virtual keys.
	OIDC OIDCConfig `json:"oidc"`
}

// OIDCConfig validates JWT bearer tokens from an identity provider. Each
// distinct token subject becomes a dynamic identity with the default limits.
type OIDCConfig struct {
	Enabled bool `json:"enabled"`
	// JWKSURL is the provider's JSON Web Key Set endpoint.
	JWKSURL string `json:"jwks_url"`
	// Issuer, when set, must match the token's "iss" claim.
	Issuer string `json:"issuer"`
	// Audience, when set, must be present in the token's "aud" claim.
	Audience string `json:"audience"`
	// DefaultRPM / DefaultMaxTokens are applied per authenticated subject.
	DefaultRPM       int   `json:"default_rpm"`
	DefaultMaxTokens int64 `json:"default_max_tokens"`
}

// VirtualKey is a client-facing credential with its own limits and budget.
// Clients send it as "Authorization: Bearer <key>". It is decoupled from the
// upstream provider keys, so you can rotate/revoke per consumer.
type VirtualKey struct {
	// Key is the secret the client sends. May be "env:NAME".
	Key string `json:"key"`
	// Name is a human label for dashboards/metrics.
	Name string `json:"name"`
	// RPM is the requests-per-minute ceiling (0 = unlimited).
	RPM int `json:"rpm"`
	// MaxTokens caps cumulative total tokens this key may spend (0 =
	// unlimited). Resets only on restart; meant as a hard budget guardrail.
	MaxTokens int64 `json:"max_tokens"`
	// AllowedModels, when non-empty, restricts which model aliases this key
	// may call.
	AllowedModels []string `json:"allowed_models,omitempty"`
}

// Provider describes how to reach one upstream API.
type Provider struct {
	// Kind selects the adapter: "openai", "anthropic", or "mock".
	Kind string `json:"kind"`
	// BaseURL overrides the adapter default (useful for Azure, Ollama,
	// Groq, or any OpenAI-compatible endpoint).
	BaseURL string `json:"base_url,omitempty"`
	// Keys is the pool of API keys. Each may be "env:NAME" to read from the
	// environment. Weighted load balancing picks among them.
	Keys []Key `json:"keys"`
}

// Key is one credential with a relative selection weight.
type Key struct {
	Value  string `json:"value"`
	Weight int    `json:"weight,omitempty"` // defaults to 1
}

// ModelRoute is an ordered list of concrete targets for a model alias.
type ModelRoute struct {
	Targets []Target `json:"targets"`
}

// Target names a provider and the upstream model id to send it.
type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// CacheConfig controls response caching.
type CacheConfig struct {
	Enabled bool `json:"enabled"`
	// TTLSeconds is how long entries live.
	TTLSeconds int `json:"ttl_seconds"`
	// MaxEntries bounds memory; oldest entries are evicted past this.
	MaxEntries int `json:"max_entries"`
	// SimilarityThreshold in [0,1]; >0 enables semantic-lite matching on
	// the final user message. 0 means exact-match only.
	SimilarityThreshold float64 `json:"similarity_threshold"`
}

// CompressionConfig controls the token-saving middleware.
type CompressionConfig struct {
	Enabled bool `json:"enabled"`
	// Strategy: "off", "trim", "window", or "summary".
	//   trim    - normalize whitespace only (lossless-ish).
	//   window  - keep system messages + the most recent turns under budget.
	//   summary - like window, but summarize the dropped history into one
	//             system note using a cheap model.
	Strategy string `json:"strategy"`
	// MaxPromptTokens is the budget that triggers compression. 0 disables
	// the trigger (compression only runs when over budget).
	MaxPromptTokens int `json:"max_prompt_tokens"`
	// KeepRecentTurns is how many trailing messages "window"/"summary"
	// always preserve verbatim.
	KeepRecentTurns int `json:"keep_recent_turns"`
	// SummaryModel is the model alias used to summarize dropped history
	// when Strategy is "summary".
	SummaryModel string `json:"summary_model"`
}

// Load reads and parses a config file, applying defaults and resolving
// env: secrets.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	for name, p := range c.Providers {
		for i := range p.Keys {
			p.Keys[i].Value = resolveSecret(p.Keys[i].Value)
			if p.Keys[i].Weight <= 0 {
				p.Keys[i].Weight = 1
			}
		}
		c.Providers[name] = p
	}
	if c.Cache.TTLSeconds == 0 {
		c.Cache.TTLSeconds = 300
	}
	if c.Cache.MaxEntries == 0 {
		c.Cache.MaxEntries = 1000
	}
	if c.Compression.KeepRecentTurns == 0 {
		c.Compression.KeepRecentTurns = 6
	}
	if c.Compression.Strategy == "" {
		c.Compression.Strategy = "window"
	}
	for i := range c.Governance.VirtualKeys {
		c.Governance.VirtualKeys[i].Key = resolveSecret(c.Governance.VirtualKeys[i].Key)
	}
	if c.MCP.MaxToolIterations == 0 {
		c.MCP.MaxToolIterations = 4
	}
	if c.Persistence.FlushSeconds == 0 {
		c.Persistence.FlushSeconds = 10
	}
	if c.Persistence.Path == "" {
		c.Persistence.Path = "frostgate-state.json"
	}
	if c.Cluster.NodeID == "" {
		if h, err := os.Hostname(); err == nil {
			c.Cluster.NodeID = h
		} else {
			c.Cluster.NodeID = "node-1"
		}
	}
	return &c, nil
}

// resolveSecret expands "env:NAME" to the environment value; other strings
// are returned unchanged.
func resolveSecret(v string) string {
	if strings.HasPrefix(v, "env:") {
		return os.Getenv(strings.TrimPrefix(v, "env:"))
	}
	return v
}
