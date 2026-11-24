package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AsatovDias/trimbar/internal/anthropic"
	"github.com/AsatovDias/trimbar/internal/config"
	"github.com/AsatovDias/trimbar/internal/gitmeta"
	"github.com/AsatovDias/trimbar/internal/pricing"
	"github.com/AsatovDias/trimbar/internal/proxy"
	"github.com/AsatovDias/trimbar/internal/storage"
)

func TestEndToEndWiringAndCacheMissAnomaly(t *testing.T) {
	repoDir := t.TempDir()
	dotGit := filepath.Join(repoDir, ".git")
	if err := os.MkdirAll(dotGit, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dotGit, "HEAD"), []byte("ref: refs/heads/feat/mvp\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dotGit, "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/trimbar/trimbar.git\n"), 0o644)

	dbPath := filepath.Join(t.TempDir(), "trimbar.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	repo := storage.NewRepository(db)
	resolver := gitmeta.NewResolver()
	ctx := context.Background()

	// Turn 1: Warm prompt cache hit
	ev1 := proxy.ProxyEvent{
		Timestamp:   time.Now().UTC().Add(-10 * time.Second),
		Method:      "POST",
		Path:        "/v1/messages",
		StatusCode:  200,
		IsStreaming: true,
		Latency:     900 * time.Millisecond,
		Inspection: &anthropic.InspectionResult{
			Model:            "claude-sonnet-4-20250514",
			AgentRole:        anthropic.AgentRoleMain,
			SystemPromptText: "Working directory: " + repoDir,
			FirstUserText:    "Build trimbar CLI",
			SystemEstTokens:  1200,
			ToolsEstTokens:   3000,
		},
		StreamResult: &anthropic.StreamResult{
			MessageID: "msg_turn_1",
			Model:     "claude-sonnet-4-20250514",
			Usage: anthropic.Usage{
				InputTokens:              100,
				CacheCreationInputTokens: 200,
				CacheReadInputTokens:     20_000,
				OutputTokens:             120,
			},
		},
		Cost: pricing.CostBreakdown{PricingKnown: true, TotalCostUSD: 0.012, NetCacheSavingsUSD: 0.05},
	}
	rec1 := buildRequestLog(ev1, resolver, repo)
	if _, err := repo.InsertRequest(ctx, rec1); err != nil {
		t.Fatalf("InsertRequest turn 1: %v", err)
	}
	if rec1.GitRepo != "trimbar/trimbar" || rec1.GitBranch != "feat/mvp" {
		t.Errorf("turn 1 git attribution = (%q, %q), want (trimbar/trimbar, feat/mvp)", rec1.GitRepo, rec1.GitBranch)
	}
	if rec1.CacheMissAnomaly {
		t.Errorf("turn 1 should not be a cache miss anomaly")
	}

	// Turn 2: Sudden mid-session cache invalidation
	ev2 := ev1
	ev2.Timestamp = time.Now().UTC()
	ev2.StreamResult = &anthropic.StreamResult{
		MessageID: "msg_turn_2",
		Model:     "claude-sonnet-4-20250514",
		Usage: anthropic.Usage{
			InputTokens:              150,
			CacheCreationInputTokens: 20_500,
			CacheReadInputTokens:     300,
			OutputTokens:             90,
		},
	}
	rec2 := buildRequestLog(ev2, resolver, repo)
	if _, err := repo.InsertRequest(ctx, rec2); err != nil {
		t.Fatalf("InsertRequest turn 2: %v", err)
	}
	if !rec2.CacheMissAnomaly {
		t.Errorf("expected turn 2 to trigger CacheMissAnomaly")
	}
	if rec2.SessionID != rec1.SessionID {
		t.Errorf("expected same session_id across turns: %q vs %q", rec1.SessionID, rec2.SessionID)
	}
}

func TestNonLoopbackHostSecurityWarning(t *testing.T) {
	loopbackHosts := []string{"127.0.0.1", "localhost", "::1", "[::1]", "127.0.0.2"}
	for _, h := range loopbackHosts {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
		if w := nonLoopbackSecurityWarning(h); w != "" {
			t.Errorf("expected no warning for loopback host %q, got %q", h, w)
		}
	}

	exposedHosts := []string{"0.0.0.0", "::", "192.168.1.10", "10.0.0.5"}
	for _, h := range exposedHosts {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
		w := nonLoopbackSecurityWarning(h)
		if !strings.Contains(w, "SECURITY WARNING") || !strings.Contains(w, "Authorization") || !strings.Contains(w, "x-api-key") {
			t.Errorf("expected Authorization/x-api-key security warning for %q, got %q", h, w)
		}
	}
}

func TestSQLiteWriteFailureDoesNotBreakProxyResponse(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "readonly.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	// Put SQLite into strict read-only mode to simulate a read-only disk / permission error.
	if _, err := db.SQL().Exec("PRAGMA query_only = ON;"); err != nil {
		t.Fatalf("PRAGMA query_only = ON: %v", err)
	}

	repo := storage.NewRepository(db)
	resolver := gitmeta.NewResolver()

	ssePayload := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_ro_1","model":"claude-sonnet-4-20250514","usage":{"input_tokens":20,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ssePayload)
	}))
	defer upstream.Close()

	var warnBuf bytes.Buffer
	doneCh := make(chan struct{}, 1)

	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL
	srv, err := proxy.NewServer(cfg, proxy.WithOnComplete(func(ev proxy.ProxyEvent) {
		rec := buildRequestLog(ev, resolver, repo)
		persistRequestLog(repo, rec, &warnBuf)
		doneCh <- struct{}{}
	}))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxySrv := httptest.NewServer(srv.ServeMux())
	defer proxySrv.Close()

	resp, err := http.Post(
		proxySrv.URL+"/v1/messages",
		"application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-20250514","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	)
	if err != nil {
		t.Fatalf("POST through proxy failed despite SQLite error being non-fatal: %v", err)
	}
	gotBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if string(gotBody) != ssePayload {
		t.Errorf("SSE response body mismatch when SQLite write fails")
	}

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for onComplete")
	}
	if !strings.Contains(warnBuf.String(), "failed to persist request") {
		t.Errorf("expected SQLite warning to be logged, got %q", warnBuf.String())
	}
}
