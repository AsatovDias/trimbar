# Trimbar

**Local-first Anthropic API reverse proxy and context telemetry CLI for Claude Code.**

Trimbar sits transparently between the local [Claude Code](https://docs.anthropic.com/en/docs/claude-code) CLI and `https://api.anthropic.com` — with **zero SSE buffering** (`<5ms` added latency), **zero external telemetry**, and **zero CGO** (single static Go binary backed by local SQLite in WAL mode).

---

## The Problem

When running Claude Code on real repositories, token burn and latency spikes are hard to diagnose:

- **Silent Prompt Cache Invalidations:** A mid-session cache miss forces Anthropic to re-bill your entire conversation prefix at `1.25×` cache-write rates instead of `0.1×` cache-read rates — turning a `$0.02` turn into a `$0.10+` spike without warning.
- **Context Bloat & Redundant Tool Calls:** Agents frequently re-`Read` unchanged files across turns or ingest raw ANSI terminal color codes from `Bash` test runners into `tool_result` blocks.
- **No Branch or Agent Attribution:** Main Agent turns (`Sonnet` / `Opus`) are mixed with background sub-agent calls (`Haiku` file search / bash summarization), and Anthropic's console doesn't know which local Git repository or branch incurred the spend.
- **Inflated Savings Math:** Naive calculators multiply wasted tokens by full uncached input prices even when 90% of the prompt is served from cache.

## What Trimbar Does

- **Zero-Buffer SSE Forwarding:** Streams chunks from Anthropic to Claude Code with immediate `http.Flusher` writes (`<5ms` proxy overhead) while asynchronously parsing usage events in a separate goroutine.
- **Exact Prompt Cache & Cost Accounting:** Extracts `input_tokens`, `cache_creation_input_tokens` (5m & 1h TTL), `cache_read_input_tokens`, and `output_tokens` from Anthropic SSE streams and non-streaming responses, computing exact USD spend, net cache savings, and **honest shadow savings** weighted by your actual blended prompt rate.
- **Context Inspector:** Detects redundant `Read` calls (ignoring legitimate re-reads after `Edit`/`Write` and distinct `offset`/`limit` slices) and ANSI CSI/OSC escape sequence overhead inside `tool_result` blocks.
- **Cache Miss Anomaly Detection:** Tracks consecutive main-agent turns per session and flags sudden drops in cache hit rate accompanied by large cache-creation spikes.
- **Automatic Git & Role Attribution:** Extracts `cwd` from Claude Code's system prompt and resolves the local Git repository and branch directly from `.git` (including worktrees and detached HEADs) — no agent configuration required. Distinguishes `[main]` agent turns from `[sub]` background Haiku sub-agents.
- **Transparent Pass-Through:** Preserves all headers (`x-api-key`, OAuth `Authorization`, `anthropic-beta`, `anthropic-version`) and passes upstream errors (`429 Too Many Requests`, `529 Overloaded`, `Retry-After`, `anthropic-ratelimit-*`) through verbatim so Claude Code's own retry logic works untouched.
- **100% Local-First:** Zero external calls other than forwarding your requests to `api.anthropic.com`. Everything stays in `~/.trimbar/trimbar.db`.

---

## Quickstart

### Download Prebuilt Binary (Windows)

Download `trimbar.exe` (windows-amd64) directly from the [GitHub Releases](https://github.com/AsatovDias/trimbar/releases/latest) page.

### Build from Source

Requires **Go 1.23+** (no C compiler / CGO required):

```bash
git clone https://github.com/AsatovDias/trimbar.git
cd trimbar
CGO_ENABLED=0 go build -o trimbar ./cmd/trimbar
```

*(On Windows PowerShell: `$env:CGO_ENABLED="0"; go build -o trimbar.exe ./cmd/trimbar`)*

### Use with Claude Code

Start the local proxy in one terminal:

```bash
./trimbar start
```

Launch Claude Code pointed at Trimbar in another terminal:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 claude
```

Inspect your captured sessions at any time:

```bash
./trimbar stats --since 24h
./trimbar inspect --limit 20
```

---

## Try Without an API Key (`trimbar demo`)

Don't have an Anthropic API key handy? Run the built-in 6-turn Claude Code simulator through the real local proxy and mock Anthropic SSE server:

```bash
./trimbar demo
./trimbar stats
./trimbar inspect
```

Want to watch live streaming telemetry inside `trimbar start`? Open two terminals:

```bash
# Terminal 1: start proxy backed by the built-in local mock Anthropic API
./trimbar start --mock

# Terminal 2: send 6 simulated Claude Code turns through the running proxy
./trimbar demo --proxy http://127.0.0.1:8787
```

---

## Example CLI Output

> **Tip:** You can reproduce this exact live output locally in one command — **no Anthropic API key required** — by running `trimbar demo` followed by `trimbar stats` and `trimbar inspect`.

### `trimbar stats`

```text
TRIMBAR STATS window=all

╭──────────────────────────╮ ╭────────────────────╮ ╭──────────────────────╮ ╭─────────────────────────╮
│  REQUESTS                │ │  TOTAL SPEND       │ │  CACHE HIT RATE      │ │  NET CACHE SAVINGS      │
│  6                       │ │  $0.2575           │ │  55.9%               │ │  $0.1865                │
│  main 5 · sub 1 · err 0  │ │  avg latency 34ms  │ │  ██████████░░░░░░░░  │ │  shadow waste ~$0.0031  │
╰──────────────────────────╯ ╰────────────────────╯ ╰──────────────────────╯ ╰─────────────────────────╯

TOKENS  input=3.6k  cache_read=65.7k  cache_write=48.2k  output=2.5k

BY MODEL
MODEL                          REQS     READ    WRITE      OUT    HIT%       COST
─────────────────────────────────────────────────────────────────────────────────
claude-sonnet-4-20250514          4    40.3k    47.3k     1.6k   45.2%    $0.2185
claude-opus-4-5-20251101          1    25.4k      900      680   95.8%    $0.0364
claude-haiku-4-5-20251001         1        0        0      140    0.0%    $0.0026

BY GIT REPO & BRANCH
REPO                         BRANCH                 REQS  MAIN/SUB    HIT%       COST
─────────────────────────────────────────────────────────────────────────────────────
trimbar/trimbar              main                      6    5/1      55.9%    $0.2575

CONTEXT HEALTH & ANOMALIES
  Cache Miss Anomalies:   1
  Duplicate Read Calls:   3 (2.2k est. wasted tokens)
  ANSI Escape Overhead:   66 est. wasted tokens
  Honest Shadow Savings:  $0.0031
```

### `trimbar inspect`

```text
TRIMBAR INSPECT (6 turns)

CONTEXT COMPOSITION (EST. TOKENS)
  system         ██░░░░░░░░░░░░░░░░░░░░░░    8.3%  (407)
  tools          ███░░░░░░░░░░░░░░░░░░░░░   11.0%  (540)
  tool_results   ███████████████████░░░░░   77.3%  (3.8k)
  messages       █░░░░░░░░░░░░░░░░░░░░░░░    3.3%  (164)

RECENT TURNS
TIME      REPO@BRANCH          MODEL            ROLE    HIT%      COST FLAGS
────────────────────────────────────────────────────────────────────────────
13:50:33  trimbar/trimbar@main opus-4-5         main   95.8%   $0.0364
13:50:33  trimbar/trimbar@main sonnet-4         main    2.3%   $0.1004 CACHE-MISS DUP-READ:1
13:50:33  trimbar/trimbar@main sonnet-4         main   91.8%   $0.0209 DUP-READ:2 ANSI:18
13:50:32  trimbar/trimbar@main sonnet-4         main   86.9%   $0.0220 ANSI:18
13:50:32  trimbar/trimbar@main haiku-4-5        sub     0.0%   $0.0026
13:50:32  trimbar/trimbar@main sonnet-4         main    0.0%   $0.0753

DUPLICATE READ HOTSPOTS
FILE                                              REDUNDANT   WASTED TOK
────────────────────────────────────────────────────────────────────────
internal/proxy/handler.go                                 2         1.6k
internal/anthropic/sse_parser.go                          1          574

ANSI ESCAPE POLLUTION
  36 escape sequences across inspected tool_results (~66 wasted tokens)

CACHE MISS ANOMALIES
  2026-11-23 13:50:33  [demo-17954238…]  sudden prompt cache invalidation mid-session (hit 92% -> 2%, +24.8k cache write)
```

Both `trimbar stats` and `trimbar inspect` also support `--repo`, `--branch`, `--session`, `--since` (`24h`, `7d`, `all`), `--db`, and `--json` for scripting.

---

## Architecture

```text
Claude Code CLI
      │  (ANTHROPIC_BASE_URL=http://127.0.0.1:8787)
      ▼
┌──────────────────────────────────────────────────────────┐
│  Trimbar Local Proxy  (internal/proxy)                   │
│  ├─ Request Cloner + Concurrent Inspector (0ms to TTFB)  │
│  ├─ Zero-Buffer SSE Tee (Flush per chunk, <5ms latency)  │
│  └─ Verbatim Error Pass-Through (429 / 529 / Headers)    │
└───────────┬───────────────────────────────┬──────────────┘
            │                               │ (async)
            ▼                               ▼
   api.anthropic.com         ┌─────────────────────────────┐
                             │ Telemetry Pipeline          │
                             │ ├─ anthropic.SSEParser      │
                             │ ├─ anthropic.Inspector      │
                             │ ├─ gitmeta.Resolver (.git)  │
                             │ ├─ pricing.Calculator       │
                             │ └─ SQLite WAL (~/.trimbar)  │
                             └─────────────────────────────┘
```

### Project Layout

- `cmd/trimbar/` — Cobra CLI (`start`, `stats`, `inspect`, `demo`, `version`) with Lipgloss tables & progress bars.
- `internal/config/` — Defaults, `~/.trimbar/config.toml` & `TRIMBAR_*` env loader, Anthropic pricing table.
- `internal/proxy/` — HTTP server, header-preserving reverse proxy, zero-buffer SSE tee forwarder.
- `internal/anthropic/` — Anthropic `/v1/messages` request/SSE types, stateful SSE parser, duplicate `Read` & ANSI detector, Cache Miss Anomaly detector.
- `internal/gitmeta/` — Extracts `cwd` from Claude Code system prompts and resolves `.git` repo, branch, worktrees, and stable `session_id`.
- `internal/pricing/` — Exact Anthropic USD cost, net cache savings, and honest shadow-savings calculator.
- `internal/storage/` — Pure-Go SQLite (`modernc.org/sqlite`) in WAL mode with schema migrations and analytical queries.

---

## Configuration

Trimbar creates `~/.trimbar/` (`0700` permissions) automatically and stores `trimbar.db` inside it. You can optionally place `~/.trimbar/config.toml` or override via environment variables:

```toml
# ~/.trimbar/config.toml
host = "127.0.0.1"
port = 8787
upstream_url = "https://api.anthropic.com"
```

Supported environment variables: `TRIMBAR_HOST`, `TRIMBAR_PORT`, `TRIMBAR_UPSTREAM_URL`, `TRIMBAR_DATA_DIR`.

---

## Development & Tests

```bash
go vet ./...
go test -v -count=1 ./...
```

The test suite verifies:
- Zero-buffering SSE forwarding latency (`<50ms` per chunk, verified non-buffered delivery)
- Fragmented SSE & prompt cache token parsing (`input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `output_tokens`, 5m/1h TTL)
- Duplicate `Read` detection (with `Edit`/`Write` invalidation & `offset`/`limit` awareness) and ANSI escape detection
- Verbatim `429` & `529` error and header pass-through
- Git repo/branch/worktree & `session_id` resolution
- SQLite WAL mode initialization, stats aggregation, and end-to-end `trimbar demo` pipeline

---

## Roadmap

- [x] **Phase 0 — Transparent Gateway:** Zero-buffer SSE reverse proxy, header preservation, verbatim `429`/`529` error pass-through, Anthropic SSE usage & cache parser.
- [x] **Phase 1 — Observability MVP:** Pure-Go SQLite WAL storage, automatic Git repo/branch & `session_id` attribution, Main vs Sub-agent classification, duplicate `Read` & ANSI detectors, Cache Miss Anomaly alerts, `trimbar start` / `stats` / `inspect` / `demo` CLI.
- [ ] **Phase 2 — Diagnostics & TUI:** Interactive Bubble Tea live dashboard (`trimbar top`), MCP tool-schema diffing to pinpoint the exact root cause of Cache Miss Anomalies, budget alerts per branch/day.
- [ ] **Phase 3 — Opt-In Active Trimming:** Safe, cache-prefix-preserving opt-in transforms (stripping ANSI escape noise from new `Bash` tool results before cache write, configurable large-output truncation warnings) that never break prompt cache hits.
- [ ] **Phase 4 — Ecosystem:** Homebrew / Scoop / GitHub Releases prebuilt binaries for macOS, Linux, and Windows, plus OpenAI/Bedrock/Vertex Anthropic endpoint support.

---

## License

Licensed under the [Apache License 2.0](LICENSE).
