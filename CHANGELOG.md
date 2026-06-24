# Changelog

All notable changes are documented here. Releases follow `vMAJOR.MINOR.PATCH` semver; beta releases append a `b` suffix (e.g. `v0.1.1b`).

---

## v0.1.3b — 2026-06-25

### Added
- **Startup update check** — frostcode now checks for a newer release in the background when it starts. If one is found, a one-line hint is shown right after the banner: `update available: v0.1.3b → v0.1.4b  (run /update to install)`. The check runs concurrently with startup and waits at most 2 s so it never stalls the REPL. Silently skipped on network errors or when already on the latest release.

---

## v0.1.2b — 2026-06-25

### Fixed
- **`addTrust()` errors surfaced** — directory creation and file write failures now print an error message instead of silently doing nothing.
- **`grep` truncation notice** — when results hit the 200-match cap, a notice is appended so the agent knows results are incomplete.
- **`fetch_url` truncation notice** — responses over 1 MiB now include a notice that the body was cut short.
- **Undo stack cleared on `/compact` warns the user** — if undo history exists when compacting, a message is printed before it is discarded.
- **Skill dedup** — loading the same skill twice with `/skill <name>` is now a no-op with an info message; the system prompt no longer grows unboundedly.
- **MCP protocol version extracted to constant** — `mcpProtocolVersion = "2024-11-05"` in `mcp/manager.go`; one place to update when the spec version changes.
- **`captureFile` preserves original file permissions** — undo now restores the file's original mode instead of always writing `0o644`.
- **Wrong repo name in update 404 error** — fixed `github.com/bob/frostcode` → `github.com/Trancezzzz/frostcode`.
- **Package comment updated** in `tools.go` to mention mode note injection and goal persistence.

---

## v0.1.1b — 2026-06-25

### Fixed
- **Version detection** — `/update` now uses proper semver comparison instead of string equality, so builds ahead of the latest release are no longer incorrectly flagged as outdated.
- **Dev builds** — dev/local builds no longer fall into the update flow; `/update` prints `"dev build — latest release is vX.Y.Z"` instead.
- **Up-to-date message** — when already on the latest release, frostcode now says `"you're up to date! frostcode vX.Y.Z is the latest release"`.

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
