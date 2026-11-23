package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookupPricing(t *testing.T) {
	cfg := Default()

	tests := []struct {
		model     string
		wantOK    bool
		wantInput float64
		wantRead  float64
	}{
		{"claude-sonnet-4-20250514", true, 3.00, 0.30},
		{"claude-sonnet-4-5-20250929", true, 3.00, 0.30},
		{"claude-3-7-sonnet-20250219", true, 3.00, 0.30},
		{"claude-3-5-sonnet-20241022", true, 3.00, 0.30},
		{"claude-opus-4-20250514", true, 15.00, 1.50},
		{"claude-opus-4-1-20250805", true, 15.00, 1.50},
		{"claude-opus-4-5-20251101", true, 5.00, 0.50},
		{"claude-opus-4-6", true, 5.00, 0.50},
		{"claude-haiku-4-5-20251001", true, 1.00, 0.10},
		{"claude-3-5-haiku-20241022", true, 0.80, 0.08},
		{"claude-3-haiku-20240307", true, 0.25, 0.03},
		{"unknown-model-v1", false, 0, 0},
	}

	for _, tc := range tests {
		p, ok := cfg.LookupPricing(tc.model)
		if ok != tc.wantOK {
			t.Errorf("LookupPricing(%q) ok = %v, want %v", tc.model, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if p.InputPerMTok != tc.wantInput || p.CacheReadPerMTok != tc.wantRead {
			t.Errorf("LookupPricing(%q) = (in=%v, read=%v), want (in=%v, read=%v)",
				tc.model, p.InputPerMTok, p.CacheReadPerMTok, tc.wantInput, tc.wantRead)
		}
	}
}

func TestLoadFromTOMLAndEnv(t *testing.T) {
	dir := t.TempDir()
	tomlContent := `
# Trimbar local config
host = "127.0.0.1"
port = 9091
upstream_url = "https://example.anthropic.test/"
fallback_upstream = "https://backup.anthropic.test/"
fallback_api_key = "sk-ant-fallback-123"
max_retries = 3
trim_enabled = true
`
	if err := os.WriteFile(filepath.Join(dir, DefaultConfigFile), []byte(tomlContent), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	t.Setenv("TRIMBAR_DATA_DIR", dir)
	t.Setenv("TRIMBAR_PORT", "9099")
	t.Setenv("TRIMBAR_UPSTREAM_URL", "")
	t.Setenv("TRIMBAR_HOST", "")
	t.Setenv("TRIMBAR_TRIM_ENABLED", "")
	t.Setenv("TRIMBAR_FALLBACK_UPSTREAM", "")
	t.Setenv("TRIMBAR_FALLBACK_API_KEY", "")
	t.Setenv("TRIMBAR_MAX_RETRIES", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.TrimEnabled {
		t.Errorf("TrimEnabled = false, want true from config.toml")
	}
	if cfg.FallbackUpstream != "https://backup.anthropic.test" {
		t.Errorf("FallbackUpstream = %q, want https://backup.anthropic.test", cfg.FallbackUpstream)
	}
	if cfg.FallbackAPIKey != "sk-ant-fallback-123" {
		t.Errorf("FallbackAPIKey = %q, want sk-ant-fallback-123", cfg.FallbackAPIKey)
	}
	if cfg.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", cfg.MaxRetries)
	}
	if cfg.Port != 9099 {
		t.Errorf("Port = %d, want 9099 (env override)", cfg.Port)
	}
	if cfg.UpstreamURL != "https://example.anthropic.test" {
		t.Errorf("UpstreamURL = %q, want trailing slash trimmed", cfg.UpstreamURL)
	}
	if cfg.ListenAddr() != "127.0.0.1:9099" {
		t.Errorf("ListenAddr() = %q, want 127.0.0.1:9099", cfg.ListenAddr())
	}
}
