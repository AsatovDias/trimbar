package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AsatovDias/trimbar/internal/config"
	"github.com/AsatovDias/trimbar/internal/storage"
)

func TestRunDemoEndToEnd(t *testing.T) {
	// Prepare a fake local git repository for attribution
	repoDir := t.TempDir()
	dotGit := filepath.Join(repoDir, ".git")
	if err := os.MkdirAll(dotGit, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dotGit, "HEAD"), []byte("ref: refs/heads/feat/demo\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dotGit, "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/trimbar/trimbar.git\n"), 0o644)

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.DBPath = filepath.Join(cfg.DataDir, "demo.db")

	if err := runDemo(context.Background(), cfg, "", repoDir); err != nil {
		t.Fatalf("runDemo() error = %v", err)
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	repo := storage.NewRepository(db)
	stats, err := repo.GetStats(context.Background(), storage.StatsFilter{})
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}

	if stats.TotalRequests != 6 {
		t.Errorf("TotalRequests = %d, want 6", stats.TotalRequests)
	}
	if stats.MainAgentRequests != 5 || stats.SubagentRequests != 1 {
		t.Errorf("role split = (main:%d, sub:%d), want (5, 1)", stats.MainAgentRequests, stats.SubagentRequests)
	}
	if stats.CacheMissAnomalies != 1 {
		t.Errorf("CacheMissAnomalies = %d, want 1", stats.CacheMissAnomalies)
	}
	if stats.DuplicateReadCalls != 3 {
		t.Errorf("DuplicateReadCalls = %d, want 3 (2 on handler.go + 1 on sse_parser.go)", stats.DuplicateReadCalls)
	}
	if stats.ANSIWastedTokens <= 0 {
		t.Errorf("ANSIWastedTokens = %d, want > 0", stats.ANSIWastedTokens)
	}
	if stats.TotalCostUSD <= 0 || stats.TotalNetCacheSavingsUSD <= 0 {
		t.Errorf("expected positive cost & net cache savings, got cost=%v net=%v",
			stats.TotalCostUSD, stats.TotalNetCacheSavingsUSD)
	}
	if len(stats.ByRepoBranch) != 1 || stats.ByRepoBranch[0].Repo != "trimbar/trimbar" || stats.ByRepoBranch[0].Branch != "feat/demo" {
		t.Errorf("ByRepoBranch = %+v, want trimbar/trimbar@feat/demo", stats.ByRepoBranch)
	}

	inspect, err := repo.GetInspect(context.Background(), storage.StatsFilter{Limit: 10})
	if err != nil {
		t.Fatalf("GetInspect: %v", err)
	}
	if len(inspect.DuplicateHotspots) != 2 {
		t.Errorf("len(DuplicateHotspots) = %d, want 2 (%+v)", len(inspect.DuplicateHotspots), inspect.DuplicateHotspots)
	}
	if len(inspect.CacheMissAnomalies) != 1 {
		t.Errorf("len(CacheMissAnomalies) = %d, want 1", len(inspect.CacheMissAnomalies))
	}
	if stats.TotalTrimmedTokens != 0 {
		t.Errorf("Shadow Mode TotalTrimmedTokens = %d, want 0", stats.TotalTrimmedTokens)
	}
}

func TestRunDemoActiveTrimmingMode(t *testing.T) {
	repoDir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.DBPath = filepath.Join(cfg.DataDir, "demo_trim.db")
	cfg.TrimEnabled = true

	if err := runDemo(context.Background(), cfg, "", repoDir); err != nil {
		t.Fatalf("runDemo(--trim) error = %v", err)
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	repo := storage.NewRepository(db)
	stats, err := repo.GetStats(context.Background(), storage.StatsFilter{})
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.TotalTrimmedTokens <= 2000 {
		t.Errorf("Active Trimming TotalTrimmedTokens = %d, want > 2000", stats.TotalTrimmedTokens)
	}
	if stats.TotalTrimmedSavingsUSD <= 0 {
		t.Errorf("Active Trimming TotalTrimmedSavingsUSD = %v, want > 0", stats.TotalTrimmedSavingsUSD)
	}
}
