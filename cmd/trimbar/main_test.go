package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trimbar/trimbar/internal/anthropic"
	"github.com/trimbar/trimbar/internal/gitmeta"
	"github.com/trimbar/trimbar/internal/pricing"
	"github.com/trimbar/trimbar/internal/proxy"
	"github.com/trimbar/trimbar/internal/storage"
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
