package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AsatovDias/trimbar/internal/anthropic"
	"github.com/AsatovDias/trimbar/internal/config"
	"github.com/AsatovDias/trimbar/internal/proxy"
	"github.com/AsatovDias/trimbar/internal/storage"
)

func seedTestRepo(t *testing.T) (*storage.DB, *storage.Repository) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "webui_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	repo := storage.NewRepository(db)
	ctx := context.Background()

	_, err = repo.InsertRequest(ctx, &storage.RequestLog{
		Timestamp:                 time.Now().UTC(),
		SessionID:                 "sess_ui_1",
		RequestID:                 "msg_ui_1",
		Method:                    "POST",
		Path:                      "/v1/messages",
		StatusCode:                200,
		Model:                     "claude-sonnet-4-20250514",
		AgentRole:                 anthropic.AgentRoleMain,
		IsStreaming:               true,
		GitRepo:                   "AsatovDias/trimbar",
		GitBranch:                 "main",
		InputTokens:               300,
		OutputTokens:              400,
		CacheCreationTokens:       1000,
		CacheReadTokens:           20000,
		CostUSD:                   0.025,
		NetCacheSavingsUSD:        0.052,
		ShadowSavingsUSD:          0.002,
		LatencyMS:                 180,
		SystemEstTokens:           500,
		ToolsEstTokens:            700,
		MessagesEstTokens:         4000,
		ToolResultsEstTokens:      3200,
		DuplicateReadCount:        2,
		DuplicateReadWastedTokens: 1500,
		DuplicateReads: []anthropic.DuplicateRead{
			{FilePath: "internal/proxy/handler.go", Count: 3, RedundantCount: 2, WastedEstTokens: 1500},
		},
		ANSISequenceCount: 12,
		ANSIWastedTokens:  40,
		TotalWastedTokens: 1540,
		TrimmedTokens:     1540,
		TrimmedSavingsUSD: 0.00462,
		RetryCount:        1,
		FailoverUsed:      true,
	})
	if err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}
	return db, repo
}

func TestWebUIServesOfflineDashboardAndAPIs(t *testing.T) {
	db, repo := seedTestRepo(t)
	defer db.Close()

	ui := NewHandler(repo)

	// Mount on proxy.Server to verify full routing integration without hitting upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected upstream hit for %s", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL
	telemetryEvents := 0
	srv, err := proxy.NewServer(cfg,
		proxy.WithWebUI(ui),
		proxy.WithOnComplete(func(ev proxy.ProxyEvent) {
			telemetryEvents++
		}),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.ServeMux())
	defer ts.Close()

	// 0. GET / redirects 302 -> /ui and GET /favicon.ico returns 204 without telemetry or upstream hit
	noFollowClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	rootResp, err := noFollowClient.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = rootResp.Body.Close()
	if rootResp.StatusCode != http.StatusFound || rootResp.Header.Get("Location") != "/ui" {
		t.Errorf("GET / = (%d, Location=%q), want (302, /ui)", rootResp.StatusCode, rootResp.Header.Get("Location"))
	}

	favResp, err := noFollowClient.Get(ts.URL + "/favicon.ico")
	if err != nil {
		t.Fatalf("GET /favicon.ico: %v", err)
	}
	_ = favResp.Body.Close()
	if favResp.StatusCode != http.StatusNoContent {
		t.Errorf("GET /favicon.ico = %d, want 204", favResp.StatusCode)
	}

	// 1. GET /ui returns embedded offline HTML dashboard
	resp, err := http.Get(ts.URL + "/ui")
	if err != nil {
		t.Fatalf("GET /ui: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /ui Content-Type = %q, want text/html", ct)
	}
	html := string(bodyBytes)
	if !strings.Contains(html, "Context Flamegraph") || !strings.Contains(html, "/api/stats") || !strings.Contains(html, "/api/inspect") {
		t.Errorf("GET /ui HTML missing expected dashboard sections")
	}
	if !strings.Contains(html, `rel="icon" type="image/svg+xml" href="data:image/svg+xml,`) {
		t.Errorf("GET /ui HTML missing inline SVG favicon")
	}
	if !strings.Contains(html, "#C4F82A") || !strings.Contains(html, "#3D4B24") || !strings.Contains(html, "#F3F1ED") {
		t.Errorf("GET /ui HTML missing Trimbar brand SVG palette")
	}
	// Strictly verify zero external CDN script/stylesheet links for 100% offline operation.
	if strings.Contains(html, "https://cdn.") || strings.Contains(html, "https://fonts.") || strings.Contains(html, "unpkg.com") {
		t.Errorf("GET /ui HTML must not reference external CDNs")
	}

	// 2. GET /api/stats returns accurate JSON telemetry
	statsResp, err := http.Get(ts.URL + "/api/stats?since=24h&repo=AsatovDias/trimbar")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer statsResp.Body.Close()
	if statsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats status = %d, want 200", statsResp.StatusCode)
	}
	var stats storage.StatsSummary
	if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode /api/stats: %v", err)
	}
	if stats.TotalRequests != 1 || stats.TotalTrimmedTokens != 1540 || stats.TotalRetries != 1 || stats.TotalFailovers != 1 {
		t.Errorf("unexpected /api/stats payload: %+v", stats)
	}

	// 3. GET /api/inspect returns flamegraph turn & hotspot data
	inspectResp, err := http.Get(ts.URL + "/api/inspect?limit=10")
	if err != nil {
		t.Fatalf("GET /api/inspect: %v", err)
	}
	defer inspectResp.Body.Close()
	if inspectResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/inspect status = %d, want 200", inspectResp.StatusCode)
	}
	var report storage.InspectReport
	if err := json.NewDecoder(inspectResp.Body).Decode(&report); err != nil {
		t.Fatalf("decode /api/inspect: %v", err)
	}
	if len(report.Turns) != 1 || len(report.DuplicateHotspots) != 1 {
		t.Errorf("unexpected /api/inspect payload: turns=%d hotspots=%d", len(report.Turns), len(report.DuplicateHotspots))
	}
	if report.TotalTrimmedTokens != 1540 {
		t.Errorf("report.TotalTrimmedTokens = %d, want 1540", report.TotalTrimmedTokens)
	}
	if telemetryEvents != 0 {
		t.Errorf("expected 0 telemetry events for /, /favicon.ico, /ui, /api/*, got %d", telemetryEvents)
	}
}

func TestWebUIDNSRebindingProtection(t *testing.T) {
	db, repo := seedTestRepo(t)
	defer db.Close()

	ui := NewHandler(repo)

	allowedHosts := []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787", "localhost"}
	for _, host := range allowedHosts {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/stats", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		ui.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q -> status %d, want 200", host, rec.Code)
		}
	}

	foreignHosts := []string{"evil.com:8787", "attacker.localhost.evil.com:8787", "192.168.1.55:8787", ""}
	endpoints := []string{"/ui", "/api/stats", "/api/inspect"}
	for _, host := range foreignHosts {
		for _, ep := range endpoints {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787"+ep, nil)
			req.Host = host
			rec := httptest.NewRecorder()
			ui.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("DNS rebinding check Host=%q path=%q -> status %d, want 403 Forbidden", host, ep, rec.Code)
			}
		}
	}
}
