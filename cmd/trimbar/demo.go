package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/trimbar/trimbar/internal/config"
	"github.com/trimbar/trimbar/internal/gitmeta"
	"github.com/trimbar/trimbar/internal/proxy"
	"github.com/trimbar/trimbar/internal/storage"
)

const demoScenarioHeader = "X-Trimbar-Demo-Turn"

type demoTurnSpec struct {
	TurnNumber  int
	Label       string
	Model       string
	Stream      bool
	Payload     map[string]any
	Usage       demoMockUsage
	ResponseTxt string
}

type demoMockUsage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

func newDemoCmd() *cobra.Command {
	var (
		proxyURL string
		dbPath   string
		cwd      string
	)

	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Send 6 realistic simulated Claude Code requests through Trimbar without an API key",
		Long: `Runs a self-contained 6-turn Claude Code simulation through the real Trimbar HTTP proxy
(including SSE streaming, prompt cache reads, Haiku sub-agent calls, duplicate Read calls,
ANSI-polluted Bash output, and a mid-session Cache Miss Anomaly).

By default, trimbar demo launches an ephemeral local mock Anthropic server and Trimbar proxy
and persists telemetry into your SQLite database so you can immediately run:
  trimbar stats
  trimbar inspect

To watch live telemetry in a separate terminal running "trimbar start --mock", pass:
  trimbar demo --proxy http://127.0.0.1:8787`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if dbPath != "" {
				cfg.DBPath = dbPath
			}
			workDir := cwd
			if workDir == "" {
				workDir, _ = os.Getwd()
			}
			return runDemo(cmd.Context(), cfg, proxyURL, workDir)
		},
	}

	cmd.Flags().StringVar(&proxyURL, "proxy", "", "Send demo traffic to an already-running Trimbar proxy (e.g. http://127.0.0.1:8787 started with --mock)")
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to SQLite database (standalone mode; default ~/.trimbar/trimbar.db)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "Working directory injected into Claude Code system prompt (default current directory)")
	return cmd
}

func runDemo(ctx context.Context, cfg *config.Config, existingProxyURL, workDir string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	sessionID := fmt.Sprintf("demo-%d", time.Now().Unix())
	turns := buildDemoTurns(workDir, sessionID)

	targetProxyURL := strings.TrimRight(strings.TrimSpace(existingProxyURL), "/")

	if targetProxyURL == "" {
		// Standalone mode: spin up local mock Anthropic upstream + real Trimbar proxy server.
		if err := cfg.EnsureDataDir(); err != nil {
			return fmt.Errorf("ensure data dir: %w", err)
		}
		db, err := storage.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer db.Close()

		repo := storage.NewRepository(db)
		resolver := gitmeta.NewResolver()

		mockUpstream := httptest.NewServer(newMockAnthropicHandler(turns))
		defer mockUpstream.Close()

		proxyCfg := *cfg
		proxyCfg.UpstreamURL = mockUpstream.URL

		srv, err := proxy.NewServer(&proxyCfg, proxy.WithOnComplete(func(ev proxy.ProxyEvent) {
			rec := buildRequestLog(ev, resolver, repo)
			writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := repo.InsertRequest(writeCtx, rec); err != nil {
				fmt.Fprintf(os.Stderr, "%s failed to persist demo request: %v\n", styleWarn.Render("WARN"), err)
			}
			printLiveEvent(rec)
		}))
		if err != nil {
			return fmt.Errorf("create demo proxy: %w", err)
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("listen ephemeral proxy: %w", err)
		}
		proxyCtx, cancelProxy := context.WithCancel(ctx)
		serveDone := make(chan error, 1)
		go func() {
			serveDone <- srv.Serve(proxyCtx, ln)
		}()
		defer func() {
			cancelProxy()
			<-serveDone
		}()

		targetProxyURL = "http://" + ln.Addr().String()

		banner := []string{
			styleBrand.Render("TRIMBAR DEMO") + " " + styleMuted.Render("· Simulated 6-turn Claude Code Session (no API key required)"),
			"",
			fmt.Sprintf("%s  %s", styleMuted.Render("Proxy:   "), styleOK.Render(targetProxyURL)),
			fmt.Sprintf("%s  %s", styleMuted.Render("Mock API:"), mockUpstream.URL),
			fmt.Sprintf("%s  %s", styleMuted.Render("Database:"), cfg.DBPath),
			fmt.Sprintf("%s  %s", styleMuted.Render("Session: "), sessionID),
		}
		fmt.Println(styleBanner.Render(strings.Join(banner, "\n")))
		fmt.Println()
	} else {
		fmt.Printf("%s sending 6 simulated Claude Code turns to %s (session %s)\n\n",
			styleBrand.Render("TRIMBAR DEMO"), styleOK.Render(targetProxyURL), styleMuted.Render(sessionID))
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	for _, turn := range turns {
		if err := sendDemoTurn(ctx, httpClient, targetProxyURL, turn); err != nil {
			return fmt.Errorf("turn %d (%s): %w", turn.TurnNumber, turn.Label, err)
		}
		if existingProxyURL != "" {
			fmt.Printf("  %s Turn %d/6: %s\n", styleOK.Render("✓"), turn.TurnNumber, turn.Label)
		}
		time.Sleep(60 * time.Millisecond)
	}

	fmt.Println()
	fmt.Println(styleOK.Render("Demo session complete!") + " " + styleMuted.Render("Explore your captured telemetry:"))
	fmt.Println("  " + styleTitle.Render("trimbar stats"))
	fmt.Println("  " + styleTitle.Render("trimbar inspect"))
	return nil
}

func sendDemoTurn(ctx context.Context, client *http.Client, proxyURL string, turn demoTurnSpec) error {
	raw, err := json.Marshal(turn.Payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-trimbar-demo-local-only")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	req.Header.Set(demoScenarioHeader, fmt.Sprintf("%d", turn.TurnNumber))

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// newMockAnthropicHandler serves realistic Anthropic SSE streams and JSON responses
// for `trimbar demo` and `trimbar start --mock`.
func newMockAnthropicHandler(turns []demoTurnSpec) http.Handler {
	byTurn := make(map[string]demoTurnSpec, len(turns))
	for _, t := range turns {
		byTurn[fmt.Sprintf("%d", t.TurnNumber)] = t
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"input_tokens":512}`)
			return
		}

		// Parse basic model/stream from request body as fallback if header is absent.
		var reqMeta struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if r.Body != nil {
			bodyBytes, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(bodyBytes, &reqMeta)
		}

		spec, ok := byTurn[r.Header.Get(demoScenarioHeader)]
		if !ok {
			model := reqMeta.Model
			if model == "" {
				model = "claude-sonnet-4-20250514"
			}
			spec = demoTurnSpec{
				TurnNumber: 0,
				Model:      model,
				Stream:     reqMeta.Stream,
				Usage: demoMockUsage{
					InputTokens:              320,
					CacheCreationInputTokens: 1200,
					CacheReadInputTokens:     14000,
					OutputTokens:             180,
				},
				ResponseTxt: "Simulated response from Trimbar local mock Anthropic server.",
			}
		}

		msgID := fmt.Sprintf("msg_demo_%02d_%d", spec.TurnNumber, time.Now().UnixNano()%100000)

		if !spec.Stream {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req_"+msgID)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          msgID,
				"type":        "message",
				"role":        "assistant",
				"model":       spec.Model,
				"stop_reason": "end_turn",
				"content":     []map[string]string{{"type": "text", "text": spec.ResponseTxt}},
				"usage": map[string]int64{
					"input_tokens":                spec.Usage.InputTokens,
					"output_tokens":               spec.Usage.OutputTokens,
					"cache_creation_input_tokens": spec.Usage.CacheCreationInputTokens,
					"cache_read_input_tokens":     spec.Usage.CacheReadInputTokens,
				},
			})
			return
		}

		// Streaming Anthropic SSE response
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("request-id", "req_"+msgID)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		writeSSE := func(event string, payload any) {
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(b))
			if flusher != nil {
				flusher.Flush()
			}
		}

		writeSSE("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":          msgID,
				"type":        "message",
				"role":        "assistant",
				"model":       spec.Model,
				"content":     []any{},
				"stop_reason": nil,
				"usage": map[string]int64{
					"input_tokens":                spec.Usage.InputTokens,
					"cache_creation_input_tokens": spec.Usage.CacheCreationInputTokens,
					"cache_read_input_tokens":     spec.Usage.CacheReadInputTokens,
					"output_tokens":               1,
				},
			},
		})

		writeSSE("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]string{"type": "text", "text": ""},
		})

		chunks := splitWords(spec.ResponseTxt, 3)
		for _, c := range chunks {
			time.Sleep(10 * time.Millisecond)
			writeSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]string{"type": "text_delta", "text": c},
			})
		}

		writeSSE("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		})

		writeSSE("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   "end_turn",
				"stop_sequence": nil,
			},
			"usage": map[string]int64{
				"output_tokens": spec.Usage.OutputTokens,
			},
		})

		writeSSE("message_stop", map[string]string{"type": "message_stop"})
	})
}

func buildDemoTurns(cwd, sessionID string) []demoTurnSpec {
	systemPrompt := fmt.Sprintf(`You are Claude Code, Anthropic's official CLI for Claude.
Here is useful information about the environment you are running in:
<env>
Working directory: %s
Is directory a git repo: Yes
Platform: local
</env>
Follow best engineering practices and inspect files carefully before editing.`, cwd)

	tools := []map[string]any{
		{
			"name":         "Read",
			"description":  "Reads a file from the local filesystem.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]string{"type": "string"}}},
		},
		{
			"name":         "Edit",
			"description":  "Performs exact string replacements in files.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]string{"type": "string"}}},
		},
		{
			"name":         "Bash",
			"description":  "Executes a bash command in a persistent shell session.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]string{"type": "string"}}},
		},
		{
			"name":          "Grep",
			"description":   "Searches file contents using ripgrep.",
			"input_schema":  map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]string{"type": "string"}}},
			"cache_control": map[string]string{"type": "ephemeral"},
		},
	}

	userID := fmt.Sprintf("user_demo01_account_trimbar_session_%s", sessionID)
	handlerFileBody := strings.Repeat("func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { /* proxy logic */ }\n", 36)
	parserFileBody := strings.Repeat("func (p *SSEParser) Write(b []byte) (int, error) { /* incremental sse parser */ }\n", 28)

	ansiBashOutput := strings.Join([]string{
		"\x1b[1;36m=== RUN   TestStreamingZeroBufferingLatency\x1b[0m",
		"\x1b[1;32m--- PASS: TestStreamingZeroBufferingLatency (0.31s)\x1b[0m",
		"\x1b[1;36m=== RUN   TestErrorPassThrough429And529\x1b[0m",
		"\x1b[1;32m--- PASS: TestErrorPassThrough429And529 (0.01s)\x1b[0m",
		"\x1b[38;5;208mWARNING:\x1b[0m \x1b[33mcache hit dropped on turn 5\x1b[0m",
		"\x1b[31mFAIL\x1b[0m\tgithub.com/trimbar/trimbar/internal/proxy\t\x1b[2m0.42s\x1b[0m",
		"\x1b]8;;https://github.com/trimbar/trimbar\x07trimbar-docs\x1b]8;;\x07",
	}, "\n")

	sysBlocks := []map[string]any{
		{"type": "text", "text": systemPrompt, "cache_control": map[string]string{"type": "ephemeral"}},
	}

	return []demoTurnSpec{
		// Turn 1: Main Agent cold cache creation
		{
			TurnNumber: 1,
			Label:      "Main Agent (Sonnet 4) — cold cache warm-up + initial Read",
			Model:      "claude-sonnet-4-20250514",
			Stream:     true,
			Usage: demoMockUsage{
				InputTokens:              420,
				CacheCreationInputTokens: 18500,
				CacheReadInputTokens:     0,
				OutputTokens:             310,
			},
			ResponseTxt: "Reading internal/proxy/handler.go to inspect header preservation and streaming.",
			Payload: map[string]any{
				"model":      "claude-sonnet-4-20250514",
				"max_tokens": 8192,
				"stream":     true,
				"metadata":   map[string]string{"user_id": userID},
				"system":     sysBlocks,
				"tools":      tools,
				"messages": []map[string]any{
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "text", "text": "Audit streaming latency and prompt cache efficiency in Trimbar.", "cache_control": map[string]string{"type": "ephemeral"}},
						},
					},
				},
			},
		},
		// Turn 2: Background Haiku sub-agent (non-streaming)
		{
			TurnNumber: 2,
			Label:      "Sub-agent (Haiku 4.5) — background file search & summarization",
			Model:      "claude-haiku-4-5-20251001",
			Stream:     false,
			Usage: demoMockUsage{
				InputTokens:              1850,
				CacheCreationInputTokens: 0,
				CacheReadInputTokens:     0,
				OutputTokens:             140,
			},
			ResponseTxt: "Found 4 Go packages under internal/: anthropic, config, pricing, proxy.",
			Payload: map[string]any{
				"model":      "claude-haiku-4-5-20251001",
				"max_tokens": 512,
				"stream":     false,
				"metadata":   map[string]string{"user_id": userID},
				"system":     fmt.Sprintf("Working directory: %s\nSummarize matching file paths concisely.", cwd),
				"messages": []map[string]any{
					{"role": "user", "content": "List Go files referencing Flush() or SSEParser."},
				},
			},
		},
		// Turn 3: Main Agent warm cache hit + ANSI-polluted Bash output
		{
			TurnNumber: 3,
			Label:      "Main Agent (Sonnet 4) — warm cache hit (87%) + ANSI Bash test logs",
			Model:      "claude-sonnet-4-20250514",
			Stream:     true,
			Usage: demoMockUsage{
				InputTokens:              380,
				CacheCreationInputTokens: 2400,
				CacheReadInputTokens:     18500,
				OutputTokens:             420,
			},
			ResponseTxt: "Test output reviewed; inspecting handler.go for streaming flush boundaries.",
			Payload: map[string]any{
				"model":      "claude-sonnet-4-20250514",
				"max_tokens": 8192,
				"stream":     true,
				"metadata":   map[string]string{"user_id": userID},
				"system":     sysBlocks,
				"tools":      tools,
				"messages": []map[string]any{
					{"role": "user", "content": "Audit streaming latency and prompt cache efficiency in Trimbar."},
					{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "tool_use", "id": "tu_bash_1", "name": "Bash", "input": map[string]string{"command": "go test -v ./internal/proxy"}},
						},
					},
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "tool_result", "tool_use_id": "tu_bash_1", "content": ansiBashOutput, "cache_control": map[string]string{"type": "ephemeral"}},
						},
					},
				},
			},
		},
		// Turn 4: Main Agent warm cache hit (92%) + Duplicate Read calls (3x handler.go) + ANSI
		{
			TurnNumber: 4,
			Label:      "Main Agent (Sonnet 4) — warm cache hit (92%) + duplicate Read x3",
			Model:      "claude-sonnet-4-20250514",
			Stream:     true,
			Usage: demoMockUsage{
				InputTokens:              290,
				CacheCreationInputTokens: 1600,
				CacheReadInputTokens:     21200,
				OutputTokens:             510,
			},
			ResponseTxt: "Confirmed zero-buffer flushing in sse_tee.go and header pass-through in handler.go.",
			Payload: map[string]any{
				"model":      "claude-sonnet-4-20250514",
				"max_tokens": 8192,
				"stream":     true,
				"metadata":   map[string]string{"user_id": userID},
				"system":     sysBlocks,
				"tools":      tools,
				"messages": []map[string]any{
					{"role": "user", "content": "Audit streaming latency and prompt cache efficiency in Trimbar."},
					{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "tool_use", "id": "tu_read_h1", "name": "Read", "input": map[string]string{"file_path": "internal/proxy/handler.go"}},
						},
					},
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "tool_result", "tool_use_id": "tu_read_h1", "content": handlerFileBody},
						},
					},
					{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "tool_use", "id": "tu_read_h2", "name": "Read", "input": map[string]string{"file_path": "internal/proxy/handler.go"}},
							{"type": "tool_use", "id": "tu_bash_2", "name": "Bash", "input": map[string]string{"command": "go test -race ./..."}},
						},
					},
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "tool_result", "tool_use_id": "tu_read_h2", "content": handlerFileBody},
							{"type": "tool_result", "tool_use_id": "tu_bash_2", "content": ansiBashOutput},
						},
					},
					{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "tool_use", "id": "tu_read_h3", "name": "Read", "input": map[string]string{"file_path": "internal/proxy/handler.go"}},
						},
					},
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "tool_result", "tool_use_id": "tu_read_h3", "content": handlerFileBody, "cache_control": map[string]string{"type": "ephemeral"}},
						},
					},
				},
			},
		},
		// Turn 5: Main Agent — Mid-session Cache Miss Anomaly (92% -> 2.3% hit rate!) + duplicate Read on sse_parser.go
		{
			TurnNumber: 5,
			Label:      "Main Agent (Sonnet 4) — CACHE MISS ANOMALY (92% -> 2%) + duplicate Read",
			Model:      "claude-sonnet-4-20250514",
			Stream:     true,
			Usage: demoMockUsage{
				InputTokens:              450,
				CacheCreationInputTokens: 24800,
				CacheReadInputTokens:     600,
				OutputTokens:             390,
			},
			ResponseTxt: "Cache prefix invalidated after dynamic tool schema change; rebuilding prompt cache.",
			Payload: map[string]any{
				"model":      "claude-sonnet-4-20250514",
				"max_tokens": 8192,
				"stream":     true,
				"metadata":   map[string]string{"user_id": userID},
				"system":     sysBlocks,
				"tools":      tools,
				"messages": []map[string]any{
					{"role": "user", "content": "Audit streaming latency and prompt cache efficiency in Trimbar."},
					{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "tool_use", "id": "tu_read_p1", "name": "Read", "input": map[string]string{"file_path": "internal/anthropic/sse_parser.go"}},
						},
					},
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "tool_result", "tool_use_id": "tu_read_p1", "content": parserFileBody},
						},
					},
					{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "tool_use", "id": "tu_read_p2", "name": "Read", "input": map[string]string{"file_path": "internal/anthropic/sse_parser.go"}},
						},
					},
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "tool_result", "tool_use_id": "tu_read_p2", "content": parserFileBody, "cache_control": map[string]string{"type": "ephemeral"}},
						},
					},
				},
			},
		},
		// Turn 6: Main Agent (Opus 4.5) — Cache recovered (96% hit rate)
		{
			TurnNumber: 6,
			Label:      "Main Agent (Opus 4.5) — cache recovered (96% hit rate) final review",
			Model:      "claude-opus-4-5-20251101",
			Stream:     true,
			Usage: demoMockUsage{
				InputTokens:              210,
				CacheCreationInputTokens: 900,
				CacheReadInputTokens:     25400,
				OutputTokens:             680,
			},
			ResponseTxt: "Final architecture review complete: zero-buffer SSE verified and all tests pass.",
			Payload: map[string]any{
				"model":      "claude-opus-4-5-20251101",
				"max_tokens": 8192,
				"stream":     true,
				"metadata":   map[string]string{"user_id": userID},
				"system":     sysBlocks,
				"tools":      tools,
				"messages": []map[string]any{
					{
						"role": "user",
						"content": []map[string]any{
							{"type": "text", "text": "Summarize final findings and verify all checks.", "cache_control": map[string]string{"type": "ephemeral"}},
						},
					},
				},
			},
		},
	}
}

func splitWords(s string, perChunk int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{s}
	}
	var out []string
	for i := 0; i < len(words); i += perChunk {
		end := min(i+perChunk, len(words))
		piece := strings.Join(words[i:end], " ")
		if i > 0 {
			piece = " " + piece
		}
		out = append(out, piece)
	}
	return out
}
