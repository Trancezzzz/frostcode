# Changelog

All notable changes are documented here. Releases are tagged `vMAJOR.MINOR.PATCH`.

---

## v0.1.0 — 2026-06-24

### What's New
- **`/update` command** — checks GitHub for a newer release and replaces the running binary in-place; prompts before downloading; falls back to the release URL if no pre-built asset exists for the current platform.
- Initial public release of **frostcode** (coding agent) and **frostgate** (LLM gateway + dashboard).
- Multi-provider routing: Anthropic, OpenAI, Groq, NVIDIA NIM, local/mock.
- Semantic response caching, conversation compression, and token metrics.
- MCP server support (stdio + HTTP transports).
- Governance layer: virtual API keys, per-key RPM + token budgets, OIDC JWT auth.
- Session save/resume, `/compact`, `/skill`, `/plan` / `/build` mode cycling.
- Windows, macOS (amd64 + arm64), and Linux pre-built binaries via GitHub Releases.
