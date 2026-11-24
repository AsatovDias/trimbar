package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/AsatovDias/trimbar/internal/anthropic"
)

func TestOpenEnablesWALMode(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "trimbar.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	mode, err := db.JournalMode(context.Background())
	if err != nil {
		t.Fatalf("JournalMode() error = %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestRepositoryInsertStatsInspectAndSessionUsage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "trimbar.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	repo := NewRepository(db)
	ctx := context.Background()
	now := time.Now().UTC()

	records := []*RequestLog{
		{
			Timestamp:                 now.Add(-5 * time.Minute),
			SessionID:                 "sess_alpha",
			RequestID:                 "msg_1",
			Method:                    "POST",
			Path:                      "/v1/messages",
			StatusCode:                200,
			Model:                     "claude-sonnet-4-20250514",
			AgentRole:                 anthropic.AgentRoleMain,
			IsStreaming:               true,
			GitRepo:                   "trimbar/trimbar",
			GitBranch:                 "main",
			InputTokens:               500,
			OutputTokens:              200,
			CacheCreationTokens:       1000,
			CacheReadTokens:           20000,
			CostUSD:                   0.015,
			CacheSavingsUSD:           0.054,
			NetCacheSavingsUSD:        0.053,
			ShadowSavingsUSD:          0.002,
			LatencyMS:                 1200,
			TTFBMS:                    300,
			SystemEstTokens:           2000,
			ToolsEstTokens:            4000,
			MessagesEstTokens:         6000,
			ToolResultsEstTokens:      3500,
			DuplicateReadCount:        2,
			DuplicateReadWastedTokens: 800,
			DuplicateReads: []anthropic.DuplicateRead{
				{FilePath: "/repo/main.go", Count: 3, RedundantCount: 2, WastedEstTokens: 800},
			},
			ANSISequenceCount: 6,
			ANSIWastedTokens:  50,
			TotalWastedTokens: 850,
		},
		{
			Timestamp:           now.Add(-2 * time.Minute),
			SessionID:           "sess_alpha",
			RequestID:           "msg_2",
			Method:              "POST",
			Path:                "/v1/messages",
			StatusCode:          200,
			Model:               "claude-sonnet-4-20250514",
			AgentRole:           anthropic.AgentRoleMain,
			IsStreaming:         true,
			GitRepo:             "trimbar/trimbar",
			GitBranch:           "main",
			InputTokens:         300,
			OutputTokens:        150,
			CacheCreationTokens: 21000,
			CacheReadTokens:     500,
			CostUSD:             0.082,
			CacheMissAnomaly:    true,
			CacheMissReason:     "sudden prompt cache invalidation mid-session",
			LatencyMS:           2400,
			TTFBMS:              700,
		},
		{
			Timestamp:    now.Add(-1 * time.Minute),
			SessionID:    "sess_alpha",
			RequestID:    "msg_3",
			Method:       "POST",
			Path:         "/v1/messages",
			StatusCode:   200,
			Model:        "claude-haiku-4-5-20251001",
			AgentRole:    anthropic.AgentRoleSubagent,
			GitRepo:      "trimbar/trimbar",
			GitBranch:    "main",
			InputTokens:  1000,
			OutputTokens: 100,
			CostUSD:      0.0015,
			LatencyMS:    400,
		},
	}

	for _, rec := range records {
		if _, err := repo.InsertRequest(ctx, rec); err != nil {
			t.Fatalf("InsertRequest() error = %v", err)
		}
	}

	// Verify LatestUsageForSession returns the latest main-agent turn (msg_2), skipping Haiku subagent.
	latest, err := repo.LatestUsageForSession(ctx, "sess_alpha")
	if err != nil {
		t.Fatalf("LatestUsageForSession() error = %v", err)
	}
	if latest == nil || latest.CacheCreationInputTokens != 21000 || latest.CacheReadInputTokens != 500 {
		t.Errorf("LatestUsageForSession() = %+v, want {cw:21000, cr:500}", latest)
	}

	// Verify GetStats aggregation
	stats, err := repo.GetStats(ctx, StatsFilter{Repo: "trimbar/trimbar"})
	if err != nil {
		t.Fatalf("GetStats() error = %v", err)
	}
	if stats.TotalRequests != 3 || stats.MainAgentRequests != 2 || stats.SubagentRequests != 1 {
		t.Errorf("request counts = (%d, main=%d, sub=%d), want (3, 2, 1)",
			stats.TotalRequests, stats.MainAgentRequests, stats.SubagentRequests)
	}
	if stats.CacheMissAnomalies != 1 {
		t.Errorf("CacheMissAnomalies = %d, want 1", stats.CacheMissAnomalies)
	}
	if stats.DuplicateReadCalls != 2 || stats.DuplicateReadWastedTokens != 800 || stats.ANSIWastedTokens != 50 {
		t.Errorf("waste stats = (dup=%d, dupTok=%d, ansiTok=%d), want (2, 800, 50)",
			stats.DuplicateReadCalls, stats.DuplicateReadWastedTokens, stats.ANSIWastedTokens)
	}
	if len(stats.ByModel) != 2 {
		t.Errorf("len(ByModel) = %d, want 2", len(stats.ByModel))
	}
	if len(stats.ByRepoBranch) != 1 || stats.ByRepoBranch[0].Branch != "main" {
		t.Errorf("ByRepoBranch = %+v, want 1 entry on main", stats.ByRepoBranch)
	}

	// Verify GetInspect report
	inspect, err := repo.GetInspect(ctx, StatsFilter{SessionID: "sess_alpha", Limit: 10})
	if err != nil {
		t.Fatalf("GetInspect() error = %v", err)
	}
	if len(inspect.Turns) != 3 {
		t.Fatalf("len(Turns) = %d, want 3", len(inspect.Turns))
	}
	if len(inspect.CacheMissAnomalies) != 1 {
		t.Errorf("len(CacheMissAnomalies) = %d, want 1", len(inspect.CacheMissAnomalies))
	}
	if len(inspect.DuplicateHotspots) != 1 || inspect.DuplicateHotspots[0].FilePath != "/repo/main.go" {
		t.Errorf("DuplicateHotspots = %+v, want [/repo/main.go]", inspect.DuplicateHotspots)
	}
	if inspect.TotalANSISequences != 6 || inspect.TotalANSIWasted != 50 {
		t.Errorf("ANSI totals = (%d, %d), want (6, 50)", inspect.TotalANSISequences, inspect.TotalANSIWasted)
	}
}

func TestV1DatabaseMigratesSafelyToV2TrimmedColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy_v1.db")

	// Simulate an older v0.1.0 SQLite database without trimmed_tokens / trimmed_savings_usd columns.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy sqlite: %v", err)
	}
	legacySchema := `
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT '');
INSERT INTO schema_migrations (version) VALUES (1);
CREATE TABLE requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	timestamp TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	request_id TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL,
	path TEXT NOT NULL,
	status_code INTEGER NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	agent_role TEXT NOT NULL DEFAULT 'main',
	is_streaming INTEGER NOT NULL DEFAULT 0,
	git_repo TEXT NOT NULL DEFAULT '',
	git_branch TEXT NOT NULL DEFAULT '',
	cwd TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	cache_savings_usd REAL NOT NULL DEFAULT 0,
	net_cache_savings_usd REAL NOT NULL DEFAULT 0,
	shadow_savings_usd REAL NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	ttfb_ms INTEGER NOT NULL DEFAULT 0,
	cache_breakpoints INTEGER NOT NULL DEFAULT 0,
	cache_miss_anomaly INTEGER NOT NULL DEFAULT 0,
	cache_miss_reason TEXT NOT NULL DEFAULT '',
	system_est_tokens INTEGER NOT NULL DEFAULT 0,
	tools_est_tokens INTEGER NOT NULL DEFAULT 0,
	messages_est_tokens INTEGER NOT NULL DEFAULT 0,
	tool_results_est_tokens INTEGER NOT NULL DEFAULT 0,
	duplicate_read_count INTEGER NOT NULL DEFAULT 0,
	duplicate_read_wasted_tokens INTEGER NOT NULL DEFAULT 0,
	duplicate_reads_json TEXT NOT NULL DEFAULT '[]',
	ansi_sequence_count INTEGER NOT NULL DEFAULT 0,
	ansi_wasted_tokens INTEGER NOT NULL DEFAULT 0,
	total_wasted_tokens INTEGER NOT NULL DEFAULT 0,
	error_body TEXT NOT NULL DEFAULT ''
);
INSERT INTO requests (timestamp, method, path, status_code, model) VALUES ('2026-01-01T00:00:00Z', 'POST', '/v1/messages', 200, 'claude-sonnet-4-20250514');
`
	if _, err := rawDB.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy v1 schema: %v", err)
	}
	_ = rawDB.Close()

	// Opening via storage.Open must safely ALTER TABLE and preserve existing rows.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() on legacy v1 DB failed: %v", err)
	}
	defer db.Close()

	repo := NewRepository(db)
	ctx := context.Background()
	if _, err := repo.InsertRequest(ctx, &RequestLog{
		Timestamp:         time.Now().UTC(),
		Method:            "POST",
		Path:              "/v1/messages",
		StatusCode:        200,
		Model:             "claude-sonnet-4-20250514",
		TrimmedTokens:     750,
		TrimmedSavingsUSD: 0.00225,
	}); err != nil {
		t.Fatalf("InsertRequest after v2 migration failed: %v", err)
	}

	stats, err := repo.GetStats(ctx, StatsFilter{})
	if err != nil {
		t.Fatalf("GetStats after v2 migration failed: %v", err)
	}
	if stats.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2 (1 legacy + 1 new)", stats.TotalRequests)
	}
	if stats.TotalTrimmedTokens != 750 || stats.TotalTrimmedSavingsUSD <= 0 {
		t.Errorf("trimmed totals = (%d, %v), want (750, >0)", stats.TotalTrimmedTokens, stats.TotalTrimmedSavingsUSD)
	}
}
