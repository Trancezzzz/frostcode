# Frostgate

A high-performance, OpenAI-compatible **AI gateway** in pure Go (zero external
dependencies). One API in front of many LLM providers, with automatic
fallbacks, weighted API-key load balancing, response caching, and a
**token-saving context-compression mode**.

Inspired by [maximhq/bifrost](https://github.com/maximhq/bifrost); this is a
focused, readable reimplementation of its core ideas.

## Features

| Capability | Notes |
|---|---|
| OpenAI-compatible API | `POST /v1/chat/completions`, plus drop-in `/openai/...` and `/anthropic/...` paths |
| Multi-provider | OpenAI (and any OpenAI-compatible: Azure, Groq, Ollama, Together), Anthropic, and a built-in `mock` for zero-config demos |
| Automatic fallbacks | Per-model ordered target chain; retries on 429/5xx/network, short-circuits on 4xx |
| Weighted load balancing | Per-provider API-key pools with relative weights; ~constant-time selection |
| Response cache | Exact-match + optional **semantic-lite** (cosine over the final user message) with TTL + FIFO eviction |
| **Token-saving mode** | `trim` / `window` / `summary` strategies to fit a prompt-token budget — see below |
| Streaming | SSE passthrough for OpenAI; Anthropic events translated to OpenAI `chat.completion.chunk` |
| **Governance** | Virtual keys with per-key RPM rate limits (token bucket), cumulative token budgets, and model allow-lists |
| **MCP tool-calling** | Connect to Model Context Protocol servers (HTTP + stdio); the gateway discovers tools, advertises them to models, and runs an agentic execution loop |
| **OIDC / JWT auth** | Accept RS256 JWT bearer tokens validated against a JWKS endpoint, alongside static virtual keys |
| **Persistence** | File-backed store so governance budgets survive restarts |
| **Clustering** | Node identity + shared-store budgets across co-located nodes |
| **Web dashboard** | Self-contained live UI at `/` (no build step) showing counters, providers, models, and per-key usage |
| Observability | Prometheus `/metrics`, `/status` JSON, `/cluster`, `/health`, and per-response `x_frostgate` metadata |
| Secrets | `env:NAME` indirection in config keeps keys out of the file |

## Quick start (no API keys needed)

```bash
go build -o frostgate ./cmd/frostgate
./frostgate -config config.demo.json

curl localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo","messages":[{"role":"user","content":"hello"}]}'
```

For real providers, set keys and use `config.json`:

```bash
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
./frostgate -config config.json
```

## Token-saving / context compression

Configured under `compression` in the config file. It runs **before** caching
and routing, so the cache key and the upstream call reflect the shrunk prompt.

```json
"compression": {
  "enabled": true,
  "strategy": "summary",
  "max_prompt_tokens": 3000,
  "keep_recent_turns": 6,
  "summary_model": "cheap-summarizer"
}
```

Strategies:

- **`trim`** — collapse whitespace in every message. Cheap, near-lossless.
- **`window`** — when the estimated prompt exceeds `max_prompt_tokens`, keep all
  `system` messages plus the last `keep_recent_turns` messages, and replace the
  dropped middle with a short breadcrumb note.
- **`summary`** — like `window`, but the dropped middle is summarized by
  `summary_model` (a cheap model routed through this same gateway) and inserted
  as a `system` note, so old context survives in compressed form. Falls back to
  `window` if summarization is unavailable.

`max_prompt_tokens: 0` means "always compress". Tokens are estimated with a
~4-chars/token heuristic (no per-model vocab needed). Every response reports
`x_frostgate.tokens_saved` and `x_frostgate.compression`, and savings are
aggregated in `frostgate_tokens_saved_total`.

## Governance (virtual keys, rate limits, budgets)

Configured under `governance`. When `enabled`, every chat request must present a
virtual key as `Authorization: Bearer <key>`. Each key has independent limits,
decoupled from the upstream provider keys so you can issue/rotate/revoke per
consumer.

```json
"governance": {
  "enabled": true,
  "virtual_keys": [
    { "key": "env:FROSTGATE_KEY_TEAM_A", "name": "team-a", "rpm": 120, "max_tokens": 5000000 },
    { "key": "env:FROSTGATE_KEY_TEAM_B", "name": "team-b", "rpm": 30, "allowed_models": ["fast"] }
  ]
}
```

- `rpm` — requests/minute via a refilling token bucket (0 = unlimited).
- `max_tokens` — cumulative total-token budget; further calls get `403` once
  exhausted (0 = unlimited).
- `allowed_models` — optional alias allow-list; other models get `403`.

Denials map to HTTP `401` (unknown key), `429` (rate limited), and `403`
(budget / model). When `governance.enabled` is `false` the gateway is open.

## MCP tool-calling

Frostgate is an MCP **client**: it connects to Model Context Protocol servers,
discovers their tools, advertises them to models in OpenAI tool format, and —
when a model returns `tool_calls` — executes them and feeds the results back,
looping up to `max_tool_iterations` until the model produces a final answer.

```json
"mcp": {
  "enabled": true,
  "max_tool_iterations": 4,
  "servers": [
    { "name": "filesystem", "transport": "stdio",
      "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"] },
    { "name": "remote", "transport": "http", "url": "http://localhost:9000" }
  ]
}
```

- **`stdio`** transport launches the server as a subprocess and exchanges
  newline-delimited JSON-RPC over stdin/stdout (the standard MCP local model).
- **`http`** transport speaks JSON-RPC over POST.

Servers that fail to connect are logged and skipped (one bad server won't sink
the gateway). Discovered tools show in `/status` and the dashboard. Responses
that used tools report `x_frostgate.tool_calls` and `tool_iterations`, and are
not cached (their output depends on live tool state).

## OIDC / JWT authentication

In addition to static virtual keys, governance can accept RS256 JWT bearer
tokens from an identity provider, validated against its JWKS endpoint (pure
stdlib — no JWT library). Each distinct token `sub` becomes a dynamic identity
with the configured default limits.

```json
"governance": {
  "enabled": true,
  "oidc": {
    "enabled": true,
    "jwks_url": "https://your-idp/.well-known/jwks.json",
    "issuer": "https://your-idp/",
    "audience": "frostgate",
    "default_rpm": 60,
    "default_max_tokens": 2000000
  }
}
```

A bearer token with two dots is treated as a JWT (signature + `exp` + `iss` +
`aud` checked); anything else is matched against static virtual keys. Per-subject
spend appears in `/status` as `oidc:<sub>`.

## Persistence & clustering

When `persistence.enabled`, governance spend is flushed to a JSON state file
every `flush_seconds` and reloaded on startup, so token budgets survive
restarts (atomic write-and-rename, so a crash can't corrupt the file).

```json
"persistence": { "enabled": true, "path": "frostgate-state.json", "flush_seconds": 10 },
"cluster": { "enabled": true, "node_id": "node-1" }
```

Multiple co-located nodes pointed at the **same persistence path** share
budgets — that's the clustering model today; a production deployment would
implement the `store.Store` interface against Redis for cross-host sharing.
Each node reports its identity at `/cluster` and stamps `x_frostgate.node` on
responses. The `store.Store` interface (`internal/store`) is the single
extension point: implement `Load`/`Save` against any backend.

## Web dashboard

Open `http://localhost:8080/` for a live dashboard (polls `/status` every 2s):
request/cache/error/token-saved counters, configured providers and model
fallback chains, and per-virtual-key usage with budget bars. It's plain
embedded HTML+JS — no build step, no assets to serve.

`/status` returns the same data as JSON for scripting.

## Response metadata

Every non-streaming response carries an `x_frostgate` block:

```json
"x_frostgate": {
  "provider": "anthropic",
  "resolved_model": "claude-opus-4-8",
  "cache_hit": false,
  "attempts": 1,
  "tokens_saved": 420,
  "compression": "summary"
}
```

## Configuration reference

- `listen` — bind address (default `:8080`).
- `providers.<name>` — `kind` (`openai`|`anthropic`|`mock`), optional `base_url`,
  and a `keys` pool (`value` may be `env:NAME`; `weight` defaults to 1).
- `models.<alias>.targets` — ordered `{provider, model}` fallback chain.
  Requests may also use raw `provider/model` syntax (e.g. `openai/gpt-4o-mini`).
- `cache` — `enabled`, `ttl_seconds`, `max_entries`, `similarity_threshold`
  (0 = exact-only; e.g. 0.92 enables semantic-lite).
- `compression` — see above.

## Project layout

```
cmd/frostgate        entrypoint + provider factory
internal/schema      OpenAI-compatible request/response types
internal/config      JSON config + env: secret resolution
internal/provider    adapter interface + openai / anthropic / mock
internal/router      model resolution, weighted keys, fallback
internal/cache       exact + semantic-lite response cache
internal/compress    token-saving middleware (trim/window/summary)
internal/governance  virtual keys, OIDC identities, rate limits, budgets
internal/oidc        RS256 JWT verification against a JWKS endpoint
internal/mcp         MCP client (http/stdio transports) + tool catalog
internal/store       persistence: Store interface, Memory + File backends
internal/gateway     pipeline orchestration + agentic tool loop
internal/metrics     Prometheus text exposition
internal/server      HTTP handlers (chat, stream, health, metrics, dashboard, cluster)
```

## Request pipeline

```
client ─▶ auth (virtual key / OIDC JWT) ─▶ compression ─▶ cache ─▶ router (fallback + LB) ─▶ provider
              │                                                          │ ▲                    │
              │                                           MCP agentic loop└─┘ (tool calls)       │
              └───────────── spend recorded (persisted) ◀── x_frostgate metadata ◀──────────────┘
```

## Testing

```bash
go test ./...
```

## License

Apache-2.0 (matching the project that inspired it).
