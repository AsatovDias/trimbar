<p align="left">
  <img src="docs/logo.svg" width="56" height="56" alt="Trimbar Logo" align="left" style="margin-right: 14px;" />
</p>

# Trimbar

**Local-first Anthropic API reverse proxy and context telemetry CLI for Claude Code.**
<br clear="left" />

Trimbar sits transparently between the local [Claude Code](https://docs.anthropic.com/en/docs/claude-code) CLI and `https://api.anthropic.com` — with **zero SSE buffering** (`<5ms` added latency), **zero external telemetry**, and **zero CGO** (single static Go binary backed by local SQLite in WAL mode).

---

## How Trimbar Works (in Plain English)

Think of Trimbar as a **local X-ray scanner and smart filter** running right on your machine.

You point Claude Code at Trimbar with a single environment variable (`ANTHROPIC_BASE_URL=http://127.0.0.1:8787`). Every message still goes straight to your own Anthropic account and streams back word-for-word with zero delay (`<5ms`), while Trimbar inspects what is actually inside each turn — and, when you turn on `--trim`, filters out obvious junk *before* you pay for it.

### Without Trimbar vs With Trimbar

```text
WITHOUT TRIMBAR
───────────────
Claude Code ──► reads handler.go 4× unchanged + attaches colored Bash test logs (\x1b[31m...)
                  │
                  ▼
          api.anthropic.com
            • Pays for all 4 copies + raw ANSI noise every turn
            • Burns through your 5-hour Pro/Max limit (or API balance)
            • Silent mid-session cache miss spikes cost 5× — you never know why
            • Hits 429 / 529 overload → session stalls or drops


WITH TRIMBAR  (ANTHROPIC_BASE_URL=http://127.0.0.1:8787)
────────────────────────────────────────────────────────
Claude Code ──► Trimbar (127.0.0.1:8787, local binary) ──► api.anthropic.com
                  │
                  ├─ Frozen prefix (up to last cache_control): SEMANTICALLY IDENTICAL (untouched)
                  ├─ After cache_control (--trim):
                  │    • Replaces redundant Read copies with a 1-line note
                  │    • Strips raw ANSI color codes from Bash output
                  ├─ On transient 529 / short 429 before stream starts:
                  │    • Auto-retries (Retry-After / 1s→2s) or fails over to backup key
                  └─ Live Context Flamegraph in your browser → http://127.0.0.1:8787/ui
```

### 3 Things You Get Immediately

1. **See what's eating your context:** Open `http://127.0.0.1:8787/ui` (or run `trimbar inspect`) to see a visual **Context Flamegraph** of every turn — how much is System Prompt, Tools, Tool Outputs, Chat History, duplicate file reads, or ANSI noise, grouped automatically by your Git repo and branch.
2. **Stretch your 5-hour limit & API budget safely:** Turn on `trimbar start --trim` to strip ANSI terminal garbage and deduplicate unchanged file re-reads *strictly after* Anthropic's `cache_control` boundary — so you save real tokens without ever breaking your prompt cache.
3. **Keep long sessions alive on `529` / failover:** When Anthropic briefly overloads (`529 Overloaded`) before a stream begins, Trimbar quietly retries with backoff or fails over to a backup upstream/key so your coding flow isn't interrupted.

---

## The Problem

When running Claude Code on real repositories, token burn and latency spikes are hard to diagnose:

- **Silent Prompt Cache Invalidations:** A mid-session cache miss forces Anthropic to re-bill your entire conversation prefix at `1.25×` cache-write rates instead of `0.1×` cache-read rates — turning a `$0.02` turn into a `$0.10+` spike without warning.
- **Context Bloat & Redundant Tool Calls:** Agents frequently re-`Read` unchanged files across turns or ingest raw ANSI terminal color codes from `Bash` test runners into `tool_result` blocks.
- **No Branch or Agent Attribution:** Main Agent turns (`Sonnet` / `Opus`) are mixed with background sub-agent calls (`Haiku` file search / bash summarization), and Anthropic's console doesn't know which local Git repository or branch incurred the spend.
- **Inflated Savings Math:** Naive calculators multiply wasted tokens by full uncached input prices even when 90% of the prompt is served from cache.

## What Trimbar Does

- **Zero-Buffer SSE Forwarding:** Streams chunks from Anthropic to Claude Code with immediate `http.Flusher` writes (`<5ms` proxy overhead) while asynchronously parsing usage events in a separate goroutine.
- **Exact Prompt Cache & Cost Accounting:** Extracts `input_tokens`, `cache_creation_input_tokens` (5m & 1h TTL), `cache_read_input_tokens`, and `output_tokens` from Anthropic SSE streams and non-streaming responses, computing exact USD spend, net cache savings, and **honest shadow savings** weighted by your actual blended prompt rate. Unknown or future models automatically fall back to Sonnet-tier estimates marked with `~$` instead of reporting `$0`.
- **Context Inspector:** Detects redundant `Read` calls (ignoring legitimate re-reads after `Edit`/`Write` and distinct `offset`/`limit` slices) and ANSI CSI/OSC escape sequence overhead inside `tool_result` blocks.
- **Cache-Safe Active Trimming (`--trim`, opt-in pre-cache turn hygiene):** Alongside Trimbar's primary focus on prompt cache-miss diagnostics and context profiling, enabling `--trim` cleans freshly appended `tool_result` blocks *before* they enter the prompt cache (strictly **after** the frozen `cache_control` prefix) — stripping ANSI codes and deduplicating verified full-file `Read` results (`>= 200` chars) without ever mutating cached prefixes.
- **Embedded Offline Web UI (`/ui`):** Built-in dark dashboard embedded directly into the single binary via `go:embed` (zero external CDNs) with live KPI cards, `/api/stats` & `/api/inspect` endpoints, and an interactive **Context Flamegraph** showing System, Tools, Tool Outputs, Dup Read/Trimmed waste, ANSI noise, and Chat History per turn.
- **Auto-Retry & Failover (`429` / `529`):** Automatically recovers from transient Anthropic rate limits (`429`) and overload (`529`) before SSE streaming starts — retrying up to 2× with `Retry-After` / exponential backoff (`1s → 2s`) or instantly failing over to a configured `fallback_upstream` / `fallback_api_key`.
- **Cache Miss Anomaly Detection:** Tracks consecutive main-agent turns per session and flags sudden drops in cache hit rate accompanied by large cache-creation spikes.
- **Automatic Git & Role Attribution:** Extracts `cwd` from Claude Code's system prompt (with fallback to `Read`/`Glob`/`Grep`/`Edit` tool paths) and resolves the local Git repository and branch directly from `.git` (including worktrees and detached HEADs) — no agent configuration required. Distinguishes `[main]` agent turns from `[sub]` background Haiku and read-only Explore sub-agents.
- **Transparent Pass-Through:** Preserves all headers (`x-api-key`, OAuth `Authorization`, `anthropic-beta`, `anthropic-version`) and passes upstream errors (`429 Too Many Requests`, `529 Overloaded`, `Retry-After`, `anthropic-ratelimit-*`) through verbatim so Claude Code's own retry logic works untouched.
- **100% Local-First:** Zero external calls other than forwarding your requests to `api.anthropic.com`. Everything stays in `~/.trimbar/trimbar.db`.

---

## Why Trimbar vs Ecosystem Tools

Trimbar's primary focus is **Cache Miss Root-Cause Diagnostics, Context Flamegraph & Git Repo/Branch Attribution**, paired with opt-in pre-cache `--trim` hygiene and `529`/failover resilience. Here is how it compares to adjacent tools in the Claude Code ecosystem:

| Capability | **Trimbar** | `ccusage` | **RTK** | **Tamp** | OpenTelemetry | LiteLLM Proxy |
| :--- | :---: | :---: | :---: | :---: | :---: | :---: |
| Architecture | Single static Go binary (local HTTP proxy) | No (Node.js CLI reading local JSONL) | CLI-level `PreToolUse` command filter | Local HTTP proxy (`compress newest only`) | Collector + Grafana/Tempo | Python proxy (optional Postgres for multi-user tracking) |
| **Cache Miss Anomaly Diagnostics** | **Yes** | No | No | No | Manual alerts | No |
| **Context Flamegraph** (`/ui`) | **Yes** | No | No | No | Manual dashboards | No |
| **Git Repo & Branch Attribution** | **Yes (auto `.git`)** | Partial | No | No | Manual tags | Manual metadata |
| **Pre-Cache Trimming / Compression** | **Opt-in (`--trim`)** | No | Yes (shell stdout before tool return) | Yes (newest-turn compression) | No | No |
| **529 Auto-Retry & Instant Failover** | **Yes** | No | No | No | No | Yes |
| Local-only metadata SQLite (no prompt bodies stored) | **Yes** | Local logs | Stateless | Stateless | Backend-dependent | Optional logging |

---

## Installation & Quickstart

### Download Prebuilt Binaries

Prebuilt zero-CGO static binaries for macOS, Linux, and Windows (plus `checksums.txt`) are published on [GitHub Releases](https://github.com/AsatovDias/trimbar/releases/latest):

#### macOS (Apple Silicon — `arm64`)
```bash
curl -fsSL -o trimbar https://github.com/AsatovDias/trimbar/releases/latest/download/trimbar-darwin-arm64
chmod +x trimbar
xattr -d com.apple.quarantine trimbar 2>/dev/null || true
sudo mv trimbar /usr/local/bin/trimbar
```

#### macOS (Intel — `amd64`)
```bash
curl -fsSL -o trimbar https://github.com/AsatovDias/trimbar/releases/latest/download/trimbar-darwin-amd64
chmod +x trimbar
xattr -d com.apple.quarantine trimbar 2>/dev/null || true
sudo mv trimbar /usr/local/bin/trimbar
```

#### Linux (`amd64`)
```bash
curl -fsSL -o trimbar https://github.com/AsatovDias/trimbar/releases/latest/download/trimbar-linux-amd64
chmod +x trimbar
sudo mv trimbar /usr/local/bin/trimbar
```

#### Windows (`amd64` PowerShell)
```powershell
Invoke-WebRequest -Uri "https://github.com/AsatovDias/trimbar/releases/latest/download/trimbar-windows-amd64.exe" -OutFile "trimbar.exe"
.\trimbar.exe version
```

### Install via `go install`

Requires **Go 1.23+** (no C compiler / CGO required):

```bash
go install github.com/AsatovDias/trimbar/cmd/trimbar@latest
```

### Build from Source

```bash
git clone https://github.com/AsatovDias/trimbar.git
cd trimbar
CGO_ENABLED=0 go build -o trimbar ./cmd/trimbar
```

*(On Windows PowerShell: `$env:CGO_ENABLED="0"; go build -o trimbar.exe ./cmd/trimbar`)*

### Use with Claude Code

Start the local proxy in Shadow Mode (default, read-only observability):

```bash
./trimbar start
```

Or enable **Cache-Safe Active Trimming** to automatically strip ANSI noise and deduplicate redundant `Read` results after the last `cache_control` breakpoint:

```bash
./trimbar start --trim
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

## CLI Commands Reference

| Command | Description | Key Flags |
| :--- | :--- | :--- |
| `trimbar start` | Start the local zero-buffer reverse proxy and offline Web UI (`/ui`) | `--trim`, `--port` (`-p`), `--host`, `--upstream`, `--mock`, `--db` |
| `trimbar stats` | Show aggregated spend, cache hit rate, model/branch breakdown & trimmed savings | `--since` (`24h`, `7d`, `all`), `--repo`, `--branch`, `--session`, `--json`, `--db` |
| `trimbar inspect` | Inspect context composition, recent turns, duplicate `Read` hotspots & cache anomalies | `--limit` (`-n`), `--repo`, `--branch`, `--session`, `--json`, `--db` |
| `trimbar demo` | Run a simulated 6-turn Claude Code session locally (no API key required) | `--trim`, `--proxy`, `--cwd`, `--db` |
| `trimbar reset` | Clear all recorded request logs from the local SQLite database | `--yes` (`-y`), `--db` |
| `trimbar version` | Print Trimbar version and Go runtime info | — |

---

## Cache-Safe Active Trimming (`--trim`)

Trimbar's primary job is **observability and cache-miss diagnostics**: by default it runs in **Shadow Mode** (`--trim=false`) and never modifies your requests.

When you explicitly opt in with `trimbar start --trim` (or `trim_enabled = true` in `~/.trimbar/config.toml`), Trimbar performs **pre-cache turn hygiene** on freshly appended `tool_result` blocks *before* they enter Anthropic's prompt cache — without ever touching previously cached turns:

1. **Frozen Prefix Guarantee:** Trimbar locates the last `cache_control` marker in `messages` (or enforces the top-level automatic caching boundary). Everything at or before that boundary stays a **semantically identical frozen prefix** (no content or order mutations up to the last `cache_control`), preventing mid-session cache invalidations.
2. **Post-Cache Cleanup Only:** On `tool_result` blocks strictly **after** the last `cache_control` marker (whose size is also measured in Shadow Mode via `post_cache_tool_bytes`):
   - **ANSI Stripping:** Removes CSI/OSC terminal color and cursor escape codes from `Bash` output.
   - **Duplicate `Read` Deduplication:** If an identical full-file `Read` (`>= 200` chars, uncompacted, same `file_path`, no `offset`/`limit`) is physically present earlier in `messages` and the file has not been modified by `Edit`/`Write` or externally, replaces the redundant payload with:
     `[Trimbar: identical content of <file_path> already loaded earlier in context]`
3. **Honest Actual Savings:** Updates `Content-Length`, logs `trimmed_tokens`, `trimmed_savings_usd`, and `post_cache_tool_bytes` in SQLite, and reports **Actual Trimmed Tokens & USD Saved** in `trimbar stats` and `trimbar inspect`.

---

## Local Offline Web UI & Context Flamegraph

Whenever `trimbar start` is running, open the embedded local dashboard in your browser:

```text
http://127.0.0.1:8787/ui
```

- **100% Offline (`go:embed`):** Pure dark-mode HTML/CSS/JS bundled inside the Go binary with zero external CDNs or telemetry (protected against DNS rebinding via strict `Host` validation).
- **Live KPI Cards:** Total Spend, Cache Hit Rate progress bar & net cache savings, Actual Trimmed Tokens & USD Saved, and Cache Anomalies / Retry / Failover counters.
- **Interactive Context Flamegraph:** Horizontal proportional bars per turn breaking down `System`, `Tools`, `Tool Outputs`, `Dup Read / Trimmed` (striped amber), `ANSI Noise`, and `Chat History`. Hover any segment for exact token share, or click any turn to inspect duplicate `Read` files, trim savings, and anomaly details.
- **Local JSON APIs:** `GET /api/stats` and `GET /api/inspect` (supporting `?since=24h`, `?repo=...`, `?branch=...`, `?limit=...`).

---

## Resilience: Auto-Retry & Failover (`529` / `429`)

When Anthropic returns `529 Overloaded` or a transient `429 Too Many Requests` **before SSE streaming begins**, Trimbar helps keep your coding session moving:

- **Auto-Retry for Transient Spikes:** Primarily designed for brief `529 Overloaded` bursts and short per-minute `429` hiccups — executes up to `max_retries` (default `2`) safe pre-stream retries honoring short `Retry-After` headers (`<= 5s`) or exponential backoff (`1s → 2s`). Long cooldowns such as Anthropic's 5-hour Pro/Max subscription `429` rolling limit (`Retry-After > 5s`) are **not** stalled — they are passed straight through so Claude Code's native UI handles them immediately.
- **Instant Failover:** Configure `fallback_upstream` and/or `fallback_api_key` in `~/.trimbar/config.toml` to fail over immediately on `529`/`429` or primary dial errors (strictly stripping primary `x-api-key` and `Authorization` headers so credentials never leak to the fallback host).
- **Full Visibility:** Retries and failovers are logged live in the terminal (`RETRY` / `FAILOVER`) and persisted to SQLite (`retry_count`, `failover_used`) for `trimbar stats`, `trimbar inspect`, and the Web UI.

---

## Try Without an API Key (`trimbar demo`)

Don't have an Anthropic API key handy? Run the built-in 6-turn Claude Code simulator through the real local proxy and mock Anthropic SSE server:

```bash
# Shadow Mode (read-only telemetry)
./trimbar demo

# Active Trimming Mode (see real trimmed tokens & USD saved)
./trimbar demo --trim

./trimbar stats
./trimbar inspect
```

Want to preview the **live terminal stream AND the Web UI Context Flamegraph** without an API key? Open two terminals:

```bash
# Terminal 1: start proxy + Web UI backed by the built-in local mock Anthropic API
./trimbar start --mock --trim

# Terminal 2: send 6 simulated Claude Code turns through the running proxy
./trimbar demo --proxy http://127.0.0.1:8787

# Open in browser: http://127.0.0.1:8787/ui
```

---

## Example CLI Output

> **Tip:** You can reproduce this exact live output locally in one command — **no Anthropic API key required** — by running `trimbar demo --trim` (or `trimbar demo` for Shadow Mode) followed by `trimbar stats` and `trimbar inspect`.

### `trimbar stats`

```text
TRIMBAR STATS window=all

╭──────────────────────────╮ ╭────────────────────╮ ╭──────────────────────╮ ╭───────────────────────────╮
│  REQUESTS                │ │  TOTAL SPEND       │ │  CACHE HIT RATE      │ │  NET CACHE SAVINGS        │
│  6                       │ │  $0.2575           │ │  55.9%               │ │  $0.1865                  │
│  main 5 · sub 1 · err 0  │ │  avg latency 33ms  │ │  ██████████░░░░░░░░  │ │  trimmed -2.2k ($0.0066)  │
╰──────────────────────────╯ ╰────────────────────╯ ╰──────────────────────╯ ╰───────────────────────────╯

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
AsatovDias/trimbar           main                      6    5/1      55.9%    $0.2575

CONTEXT HEALTH & ANOMALIES
  Cache Miss Anomalies:            1
  Duplicate Read Calls:            3 (2.2k est. wasted tokens)
  ANSI Escape Overhead:            66 est. wasted tokens
  Honest Shadow Savings:           $0.0031
  Actual Trimmed Tokens & Savings: 2.2k tokens ($0.0066 USD saved)
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
14:14:03  AsatovDias/trimbar@… opus-4-5         main   95.8%   $0.0364
14:14:03  AsatovDias/trimbar@… sonnet-4         main    2.3%   $0.1004 TRIM:-550 CACHE-MISS DUP-READ:1
14:14:03  AsatovDias/trimbar@… sonnet-4         main   91.8%   $0.0209 TRIM:-1.6k DUP-READ:2 ANSI:18
14:14:03  AsatovDias/trimbar@… sonnet-4         main   86.9%   $0.0220 TRIM:-33 ANSI:18
14:14:03  AsatovDias/trimbar@… haiku-4-5        sub     0.0%   $0.0026
14:14:03  AsatovDias/trimbar@… sonnet-4         main    0.0%   $0.0753

DUPLICATE READ HOTSPOTS
FILE                                              REDUNDANT   WASTED TOK
────────────────────────────────────────────────────────────────────────
internal/proxy/handler.go                                 2         1.6k
internal/anthropic/sse_parser.go                          1          574

ACTIVE TRIMMING (CACHE-SAFE)
  2.2k tokens trimmed after cache_control boundary ($0.0066 actual USD saved)

ANSI ESCAPE POLLUTION
  36 escape sequences across inspected tool_results (~66 wasted tokens)

CACHE MISS ANOMALIES
  2026-11-23 14:14:03  [demo-17954252…]  sudden prompt cache invalidation mid-session (hit 92% -> 2%, +24.8k cache write)
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
- `internal/proxy/` — HTTP server, header-preserving reverse proxy, zero-buffer SSE tee forwarder, and `429`/`529` auto-retry & failover engine.
- `internal/trimmer/` — Cache-safe active request trimmer (frozen-prefix boundary enforcement, post-cache ANSI stripping & duplicate `Read` deduplication).
- `internal/webui/` — Embedded offline dark dashboard (`GET /ui` via `go:embed`) with Context Flamegraph and JSON endpoints (`/api/stats`, `/api/inspect`).
- `internal/anthropic/` — Anthropic `/v1/messages` request/SSE types, stateful SSE parser, duplicate `Read` & ANSI detector, Cache Miss Anomaly detector.
- `internal/gitmeta/` — Extracts `cwd` from Claude Code system prompts and resolves `.git` repo, branch, worktrees, and stable `session_id`.
- `internal/pricing/` — Exact Anthropic USD cost, net cache savings, honest shadow savings, and actual trimmed USD calculator.
- `internal/storage/` — Pure-Go SQLite (`modernc.org/sqlite`) in WAL mode with backwards-compatible schema migrations and analytical queries.

---

## Privacy & What SQLite Stores

Trimbar is **100% local-first** and makes zero external network calls other than proxying your own Anthropic requests to `api.anthropic.com` (or your configured `fallback_upstream`).

**What IS stored in `~/.trimbar/trimbar.db` (metadata & counters only):**
- Request timestamp, HTTP method/path/status, latency & TTFB (ms), retry & failover counts
- Model name, agent role (`main` / `subagent`), opaque `session_id`
- Exact token counts (`input`, `output`, `cache_creation`, `cache_read`, `trimmed_tokens`) and USD cost/savings figures
- Estimated section sizes (`system`, `tools`, `tool_results`, `messages` token counts) and ANSI escape counts
- File paths (names only) of duplicate `Read` hotspots and local Git repository / branch names

**What is NEVER written to disk:**
- Source code contents or file bodies returned by `Read` / `Grep` / `Bash`
- System prompt texts, user prompts, assistant responses, or thinking blocks
- Anthropic API keys (`x-api-key`) or OAuth `Authorization` tokens

---

## Configuration

Trimbar creates `~/.trimbar/` (`0700` permissions) automatically and stores `trimbar.db` inside it. You can optionally place `~/.trimbar/config.toml` or override via environment variables:

```toml
# ~/.trimbar/config.toml
host = "127.0.0.1"
port = 8787
upstream_url = "https://api.anthropic.com"
trim_enabled = false

# Optional resilience & failover settings
max_retries = 2
fallback_upstream = ""
fallback_api_key = ""
```

Supported environment variables: `TRIMBAR_HOST`, `TRIMBAR_PORT`, `TRIMBAR_UPSTREAM_URL`, `TRIMBAR_DATA_DIR`, `TRIMBAR_TRIM_ENABLED`, `TRIMBAR_MAX_RETRIES`, `TRIMBAR_FALLBACK_UPSTREAM`, `TRIMBAR_FALLBACK_API_KEY`.

---

## Development & Tests

```bash
go vet ./...
go test -v -count=1 ./...
```

The test suite verifies:
- Zero-buffering SSE forwarding latency (`<50ms` per chunk, verified non-buffered delivery)
- Fragmented SSE & prompt cache token parsing (`input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `output_tokens`, 5m/1h TTL)
- Cache-safe active trimming: semantically identical frozen prefix preservation (no content or order mutations up to the last `cache_control` or top-level automatic cache), `post_cache_tool_bytes` Shadow Mode surface metric, safety guards against `/compact` stubs / `<200`-char originals / missing originals / externally modified files, post-cache duplicate `Read` & ANSI trimming, and `Edit`/`Write` deduplication reset
- Auto-retry on transient `529`/`429` (strictly pre-stream, with exponential backoff & `Retry-After`), instant failover to `fallback_upstream` / `fallback_api_key` with auth header stripping, and verbatim error pass-through when retries are exhausted
- Embedded offline Web UI (`GET /ui` with zero external CDNs, inline SVG favicon, DNS rebinding `403` protection, `GET /` → `/ui` redirect, `/api/stats`, `/api/inspect`)
- Fallback Sonnet pricing (`Estimated: true`, `~$` display) for unknown/future models + Claude `5.x` pricing prefixes
- Read-only Explore sub-agent detection (`Read`/`Grep`/`Glob` without `Edit`/`Write`), duplicate `Read` & ANSI detection
- Git repo/branch/worktree & `session_id` resolution (including fallback from `tool_use` file paths)
- SQLite WAL mode initialization, safe v1→v5 `ALTER TABLE` migrations, read-only DB fault isolation, stats aggregation, and end-to-end `trimbar demo` / `trimbar demo --trim` pipelines

---

## Roadmap

- [x] **Phase 0 — Transparent Gateway:** Zero-buffer SSE reverse proxy, header preservation, verbatim `429`/`529` error pass-through, Anthropic SSE usage & cache parser.
- [x] **Phase 1 — Observability MVP (`v0.1.0`):** Pure-Go SQLite WAL storage, automatic Git repo/branch & `session_id` attribution, Main vs Sub-agent classification, duplicate `Read` & ANSI detectors, Cache Miss Anomaly alerts, `trimbar start` / `stats` / `inspect` / `demo` CLI.
- [x] **Phase 2 — Cache-Safe Active Trimming (`v0.2.0`):** Opt-in `--trim` pre-cache turn hygiene maintaining a semantically identical frozen `cache_control` prefix while stripping ANSI escape noise and deduplicating redundant full-file `Read` results after the cache boundary, with actual trimmed token & USD tracking.
- [x] **Phase 3 — Local Web UI & Resilience (`v0.3.0` / `v0.3.1`):** Embedded offline Web UI (`http://127.0.0.1:8787/ui`) with brand identity, interactive Context Flamegraph, JSON APIs (`/api/stats`, `/api/inspect`), and pre-stream `529`/`429` retry + failover (`fallback_upstream` / `fallback_api_key`).
- [x] **Phase 4 — Hardening, Security & Cross-Platform CI (`v0.4.0`–`v0.4.3`):** `/compact`-safe Read guards, top-level automatic cache support, failover auth stripping, DNS rebinding protection, `post_cache_tool_bytes` Shadow Mode telemetry, Sonnet fallback pricing (`~$`), Go 1.23 `go install` compatibility, per-push race CI (`ci.yml`), and automated cross-platform releases (`release.yml`).
- [ ] **Phase 5 — Deep Diagnostics & Ecosystem:** MCP tool-schema diffing to pinpoint exact root causes of Cache Miss Anomalies, daily/branch budget alerts, Homebrew / Scoop formulas, and Bedrock/Vertex Anthropic endpoint support.

---

## License

Licensed under the [Apache License 2.0](LICENSE).
