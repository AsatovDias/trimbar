# SPEC.md — Trimbar MVP (Claude Code Edition: Phase 0 & Phase 1)

## 1. Overview & Goal
**Trimbar** is an open-source, local-first HTTP reverse proxy designed specifically for **Claude Code**. It sits between the local Claude Code CLI and the Anthropic API (`https://api.anthropic.com`).

**Goal of this MVP:**
Build a single-binary local gateway in **Go** that:
1. Forwards `/v1/messages` and auxiliary Claude Code endpoints with **zero SSE buffering** (`<5ms` added latency).
2. Accurately parses Anthropic's `cache_control` structure and SSE usage events (`input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `output_tokens`).
3. Distinguishes **Main Agent turns** (Sonnet/Opus) from **Claude Code background sub-agents** (e.g., Haiku file search / bash summarization).
4. Automatically attributes every request to the local **Git repository and branch** without requiring any agent configuration.
5. Detects **Cache Miss Anomalies** (sudden prompt cache invalidations mid-session).
6. Provides a clean CLI (`trimbar start`, `trimbar stats`, `trimbar inspect`) backed by a local SQLite database.

---

## 2. Technical Stack & Constraints
- **Language:** Go 1.23+
- **Database:** SQLite via `modernc.org/sqlite` (pure Go driver — **no CGO** for effortless single-binary cross-compilation). Enable WAL mode (`PRAGMA journal_mode=WAL;`).
- **CLI Framework:** `github.com/spf13/cobra`
- **Terminal Formatting:** `github.com/charmbracelet/lipgloss` for clean CLI tables and progress bars.
- **Dependencies:** Minimal external dependencies. Use standard library `net/http`, `encoding/json`, `bufio` for proxying and SSE parsing.
- **Data Directory:** `~/.trimbar/` (auto-created on startup with `0700` permissions), storing `trimbar.db` and optional `config.toml`.
- **Privacy / Local-First Rule:** Zero external network calls other than forwarding the user's own requests to `api.anthropic.com`. All telemetry stays strictly in `~/.trimbar/trimbar.db`.

---

## 3. Directory Structure

```text
trimbar/
├── cmd/
│   └── trimbar/
│       └── main.go              # CLI entrypoint (start, stats, inspect, version)
├── internal/
│   ├── config/
│   │   └── config.go            # Port, upstream URL, Anthropic pricing table
│   ├── proxy/
│   │   ├── server.go            # HTTP server & routing
│   │   ├── handler.go           # Request cloning, header preservation, error pass-through
│   │   └── sse_tee.go           # Zero-buffer SSE stream forwarder + async parser
│   ├── anthropic/
│   │   ├── types.go             # Anthropic /v1/messages request & SSE event structs
│   │   ├── inspector.go         # Analyzes system, tools, tool_result, duplicate Read calls
│   │   └── sse_parser.go        # Stateful SSE line parser extracting exact usage
│   ├── gitmeta/
│   │   └── resolver.go          # Extracts cwd from Claude Code system prompt -> git repo & branch
│   ├── pricing/
│   │   └── calculator.go        # Anthropic USD cost & honest shadow-savings calculator
│   └── storage/
│       ├── sqlite.go            # DB initialization, schema migrations, WAL setup
│       └── repository.go        # Insert request logs, aggregate stats & inspect queries
├── SPEC.md
└── go.mod