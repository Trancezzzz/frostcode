// Package gateway wires the request pipeline together: compression -> cache ->
// router (with fallback + load balancing) -> response metadata.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"frostgate/internal/cache"
	"frostgate/internal/compress"
	"frostgate/internal/config"
	"frostgate/internal/governance"
	"frostgate/internal/mcp"
	"frostgate/internal/metrics"
	"frostgate/internal/oidc"
	"frostgate/internal/provider"
	"frostgate/internal/router"
	"frostgate/internal/schema"
	"frostgate/internal/store"
)

// Gateway is the orchestrator used by the HTTP layer.
type Gateway struct {
	Router     *router.Router
	Cache      *cache.Cache
	Compressor *compress.Compressor
	Metrics    *metrics.Metrics
	Governor   *governance.Governor
	MCP        *mcp.Manager
	Config     *config.Config
	NodeID     string
}

// New builds a Gateway from config, instantiated providers, an MCP manager,
// and a persistence store.
func New(cfg *config.Config, providers map[string]provider.Provider, mcpMgr *mcp.Manager, st store.Store) *Gateway {
	r := router.New(cfg, providers)
	m := metrics.New()

	// Build an OIDC verifier if configured; governance accepts JWTs through it.
	var verifier *oidc.Verifier
	if cfg.Governance.OIDC.Enabled {
		verifier = oidc.New(oidc.Config{
			JWKSURL:  cfg.Governance.OIDC.JWKSURL,
			Issuer:   cfg.Governance.OIDC.Issuer,
			Audience: cfg.Governance.OIDC.Audience,
		})
	}

	// The summarizer routes a normal chat request through the gateway's own
	// router using the configured cheap summary model.
	summarize := func(ctx context.Context, model string, history []schema.Message) (string, error) {
		attempts, err := r.Resolve(model)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		sb.WriteString("Summarize the following conversation concisely, preserving facts, decisions, names, and open questions. Output only the summary.\n\n")
		for _, h := range history {
			if t, ok := h.TextContent(); ok {
				sb.WriteString(h.Role)
				sb.WriteString(": ")
				sb.WriteString(t)
				sb.WriteString("\n")
			}
		}
		msg := schema.Message{Role: "user"}
		msg.SetText(sb.String())
		req := &schema.ChatRequest{Model: model, Messages: []schema.Message{msg}}
		resp, _, _, err := r.Chat(ctx, attempts, req)
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", nil
		}
		txt, _ := resp.Choices[0].Message.TextContent()
		return txt, nil
	}

	return &Gateway{
		Router:     r,
		Cache:      cache.New(cfg.Cache.Enabled, cfg.Cache.TTLSeconds, cfg.Cache.MaxEntries, cfg.Cache.SimilarityThreshold),
		Compressor: compress.New(cfg.Compression, summarize),
		Metrics:    m,
		Governor:   governance.New(cfg.Governance, verifier, st),
		MCP:        mcpMgr,
		Config:     cfg,
		NodeID:     cfg.Cluster.NodeID,
	}
}

// SetProviderKey swaps a provider's API key at runtime (in-memory). It backs
// the agent CLI's /key command. Returns false if the provider isn't configured.
func (g *Gateway) SetProviderKey(providerName, key string) bool {
	return g.Router.SetProviderKey(providerName, key)
}

// Providers lists the configured provider names.
func (g *Gateway) Providers() []string { return g.Router.Providers() }

// Complete runs the non-streaming pipeline and returns a response with gateway
// metadata attached.
func (g *Gateway) Complete(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	g.Metrics.Requests.Add(1)

	// 1. Compression (save-token mode) runs before caching so the cache key
	//    reflects the compressed prompt that will actually be sent.
	cres := g.Compressor.Apply(ctx, req)
	if cres.TokensSaved > 0 {
		g.Metrics.TokensSaved.Add(int64(cres.TokensSaved))
	}

	// 2. Cache lookup.
	if resp, ok := g.Cache.Get(req.Model, req); ok {
		g.Metrics.CacheHits.Add(1)
		attachMeta(resp, "cache", req.Model, true, 0, cres)
		resp.Gateway.NodeID = g.NodeID
		return resp, nil
	}

	// 3. Route with fallback + weighted key balancing.
	attempts, err := g.Router.Resolve(req.Model)
	if err != nil {
		g.Metrics.Errors.Add(1)
		return nil, err
	}

	// 4. Execute. When MCP tools are available, run an agentic loop so the
	//    model can call tools and the gateway feeds results back.
	var resp *schema.ChatResponse
	var won router.Attempt
	var n, toolCalls, iterations int
	if g.MCP != nil && g.MCP.Enabled() {
		injectTools(req, g.MCP.ToolsJSON())
		resp, won, n, toolCalls, iterations, err = g.runToolLoop(ctx, attempts, req)
	} else {
		resp, won, n, err = g.Router.Chat(ctx, attempts, req)
	}
	if err != nil {
		g.Metrics.Errors.Add(1)
		return nil, err
	}
	g.Metrics.ObserveProvider(won.Provider.Name())

	// 5. Stamp metadata and cache (skip caching tool-using responses, whose
	//    output depends on live tool state).
	attachMeta(resp, won.Provider.Name(), won.Model, false, n, cres)
	resp.Gateway.ToolCalls = toolCalls
	resp.Gateway.ToolIterations = iterations
	resp.Gateway.NodeID = g.NodeID
	if toolCalls == 0 {
		g.Cache.Put(req.Model, req, resp)
	}
	return resp, nil
}

// injectTools merges the MCP tool catalog into the request's tools array,
// preserving any tools the client already supplied.
func injectTools(req *schema.ChatRequest, mcpTools []byte) {
	if len(mcpTools) == 0 {
		return
	}
	if len(req.Tools) == 0 {
		req.Tools = mcpTools
		return
	}
	var existing, extra []interface{}
	if jsonUnmarshal(req.Tools, &existing) == nil && jsonUnmarshal(mcpTools, &extra) == nil {
		merged := append(existing, extra...)
		if b, err := jsonMarshal(merged); err == nil {
			req.Tools = b
		}
	}
}

// runToolLoop drives the model<->tools conversation up to MaxIterations. Each
// round: call the model; if it requests tools, execute them and append the
// results; otherwise return.
func (g *Gateway) runToolLoop(ctx context.Context, attempts []router.Attempt, req *schema.ChatRequest) (resp *schema.ChatResponse, won router.Attempt, totalAttempts, toolCalls, iterations int, err error) {
	maxIter := g.MCP.MaxIterations()
	for i := 0; i < maxIter; i++ {
		iterations = i + 1
		r, a, n, e := g.Router.Chat(ctx, attempts, req)
		totalAttempts += n
		if e != nil {
			return nil, a, totalAttempts, toolCalls, iterations, e
		}
		resp, won = r, a
		if len(r.Choices) == 0 {
			break
		}
		calls := r.Choices[0].Message.ParseToolCalls()
		if len(calls) == 0 {
			break // model produced a final answer
		}
		// Append the assistant's tool-call message, then each tool result.
		req.Messages = append(req.Messages, r.Choices[0].Message)
		for _, c := range calls {
			toolCalls++
			out, callErr := g.MCP.Call(c.Function.Name, []byte(c.Function.Arguments))
			if callErr != nil {
				out = "tool error: " + callErr.Error()
			}
			tm := schema.Message{Role: "tool", ToolCallID: c.ID}
			tm.SetText(out)
			req.Messages = append(req.Messages, tm)
		}
	}
	return resp, won, totalAttempts, toolCalls, iterations, nil
}

// PrepareStream applies compression and resolves routing for a streaming
// request. Streaming bypasses the cache.
func (g *Gateway) PrepareStream(ctx context.Context, req *schema.ChatRequest) ([]router.Attempt, error) {
	g.Metrics.Requests.Add(1)
	cres := g.Compressor.Apply(ctx, req)
	if cres.TokensSaved > 0 {
		g.Metrics.TokensSaved.Add(int64(cres.TokensSaved))
	}
	attempts, err := g.Router.Resolve(req.Model)
	if err != nil {
		g.Metrics.Errors.Add(1)
		return nil, err
	}
	return attempts, nil
}

// BuildProviders instantiates an adapter per configured provider.
func BuildProviders(cfg *config.Config) (map[string]provider.Provider, error) {
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

// FromConfig builds a fully wired Gateway plus a cleanup func. It is the
// single construction path shared by the server and the agent CLI.
func FromConfig(cfg *config.Config) (*Gateway, func(), error) {
	providers, err := BuildProviders(cfg)
	if err != nil {
		return nil, nil, err
	}
	var st store.Store
	if cfg.Persistence.Enabled {
		st = store.NewFile(cfg.Persistence.Path)
	} else {
		st = store.NewMemory()
	}
	mgr, _ := mcp.NewManager(cfg.MCP)
	gw := New(cfg, providers, mgr, st)
	return gw, func() { mgr.Close() }, nil
}

// ToolChat runs a single model turn with whatever tools are attached to the
// request, bypassing cache and compression. The agent CLI drives its own tool
// loop on top of this, so it needs the raw model output (including tool_calls)
// without gateway-side caching.
func (g *Gateway) ToolChat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	attempts, err := g.Router.Resolve(req.Model)
	if err != nil {
		return nil, err
	}
	resp, won, _, err := g.Router.Chat(ctx, attempts, req)
	if err != nil {
		return nil, err
	}
	g.Metrics.ObserveProvider(won.Provider.Name())
	if resp.Gateway == nil {
		resp.Gateway = &schema.GatewayMeta{}
	}
	resp.Gateway.Provider = won.Provider.Name()
	resp.Gateway.ResolvedModel = won.Model
	return resp, nil
}

// ToolChatStream runs a single streaming model turn, invoking onText for each
// text delta as it arrives, and returns the fully reassembled response
// (including any tool_calls) so the agent's tool loop can proceed. Like
// ToolChat it bypasses cache and compression. Streaming tool_calls require an
// OpenAI-compatible provider (OpenAI, NVIDIA NIM, Groq, ...); the Anthropic
// adapter streams text only.
func (g *Gateway) ToolChatStream(ctx context.Context, req *schema.ChatRequest, onText, onReasoning func(string)) (*schema.ChatResponse, error) {
	attempts, err := g.Router.Resolve(req.Model)
	if err != nil {
		return nil, err
	}
	req.Stream = true
	asm := newSSEAssembler(onText, onReasoning)
	won, _, err := g.Router.Stream(ctx, attempts, req, asm, func() {})
	if err != nil {
		return nil, err
	}
	g.Metrics.ObserveProvider(won.Provider.Name())
	resp := asm.Response()
	if resp.Gateway == nil {
		resp.Gateway = &schema.GatewayMeta{}
	}
	resp.Gateway.Provider = won.Provider.Name()
	resp.Gateway.ResolvedModel = won.Model
	return resp, nil
}

// json helpers (thin wrappers to keep call sites terse).
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func jsonMarshal(v any) ([]byte, error)    { return json.Marshal(v) }

// attachMeta fills the x_frostgate response metadata block.
func attachMeta(resp *schema.ChatResponse, providerName, model string, cacheHit bool, attempts int, cres compress.Result) {
	if resp.Gateway == nil {
		resp.Gateway = &schema.GatewayMeta{}
	}
	resp.Gateway.Provider = providerName
	resp.Gateway.ResolvedModel = model
	resp.Gateway.CacheHit = cacheHit
	resp.Gateway.Attempts = attempts
	resp.Gateway.TokensSaved = cres.TokensSaved
	resp.Gateway.CompressionUsed = cres.Applied
}
