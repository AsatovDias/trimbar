package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/trimbar/trimbar/internal/anthropic"
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
