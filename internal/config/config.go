package config

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultHost        = "127.0.0.1"
	DefaultPort        = 8787
	DefaultUpstreamURL = "https://api.anthropic.com"
	DefaultDataDirName = ".trimbar"
	DefaultDBFileName  = "trimbar.db"
	DefaultConfigFile  = "config.toml"
)

// ModelPricing defines Anthropic API USD pricing per 1 million tokens.
type ModelPricing struct {
	InputPerMTok        float64 // Base uncached input tokens ($ / 1M tokens)
	OutputPerMTok       float64 // Output tokens ($ / 1M tokens)
	CacheWritePerMTok   float64 // 5-minute TTL prompt cache write (1.25x base input)
	CacheWrite1hPerMTok float64 // 1-hour TTL prompt cache write (2.0x base input)
	CacheReadPerMTok    float64 // Prompt cache read (0.1x base input)
}

// Config holds runtime settings and the Anthropic pricing table for Trimbar.
type Config struct {
	Host        string
	Port        int
	UpstreamURL string
	DataDir     string
	DBPath      string
	TrimEnabled bool
	Pricing     map[string]ModelPricing
}

// DefaultPricingTable returns Anthropic's USD per-1M-token pricing table.
// Keys are canonical model prefixes matched by exact match first, then longest prefix.
func DefaultPricingTable() map[string]ModelPricing {
	return map[string]ModelPricing{
		// Claude 4.5 / 4.6 Opus ($5 / $25)
		"claude-opus-4-6": {
			InputPerMTok:        5.00,
			OutputPerMTok:       25.00,
			CacheWritePerMTok:   6.25,
			CacheWrite1hPerMTok: 10.00,
			CacheReadPerMTok:    0.50,
		},
		"claude-opus-4-5": {
			InputPerMTok:        5.00,
			OutputPerMTok:       25.00,
			CacheWritePerMTok:   6.25,
			CacheWrite1hPerMTok: 10.00,
			CacheReadPerMTok:    0.50,
		},
		// Claude 4 / 4.1 Opus ($15 / $75)
		"claude-opus-4-1": {
			InputPerMTok:        15.00,
			OutputPerMTok:       75.00,
			CacheWritePerMTok:   18.75,
			CacheWrite1hPerMTok: 30.00,
			CacheReadPerMTok:    1.50,
		},
		"claude-opus-4": {
			InputPerMTok:        15.00,
			OutputPerMTok:       75.00,
			CacheWritePerMTok:   18.75,
			CacheWrite1hPerMTok: 30.00,
			CacheReadPerMTok:    1.50,
		},
		"claude-3-opus": {
			InputPerMTok:        15.00,
			OutputPerMTok:       75.00,
			CacheWritePerMTok:   18.75,
			CacheWrite1hPerMTok: 30.00,
			CacheReadPerMTok:    1.50,
		},
		// Claude Sonnet family ($3 / $15)
		"claude-sonnet-4-6": {
			InputPerMTok:        3.00,
			OutputPerMTok:       15.00,
			CacheWritePerMTok:   3.75,
			CacheWrite1hPerMTok: 6.00,
			CacheReadPerMTok:    0.30,
		},
		"claude-sonnet-4-5": {
			InputPerMTok:        3.00,
			OutputPerMTok:       15.00,
			CacheWritePerMTok:   3.75,
			CacheWrite1hPerMTok: 6.00,
			CacheReadPerMTok:    0.30,
		},
		"claude-sonnet-4": {
			InputPerMTok:        3.00,
			OutputPerMTok:       15.00,
			CacheWritePerMTok:   3.75,
			CacheWrite1hPerMTok: 6.00,
			CacheReadPerMTok:    0.30,
		},
		"claude-3-7-sonnet": {
			InputPerMTok:        3.00,
			OutputPerMTok:       15.00,
			CacheWritePerMTok:   3.75,
			CacheWrite1hPerMTok: 6.00,
			CacheReadPerMTok:    0.30,
		},
		"claude-3-5-sonnet": {
			InputPerMTok:        3.00,
			OutputPerMTok:       15.00,
			CacheWritePerMTok:   3.75,
			CacheWrite1hPerMTok: 6.00,
			CacheReadPerMTok:    0.30,
		},
		// Claude Haiku 4.5 ($1 / $5)
		"claude-haiku-4-5": {
			InputPerMTok:        1.00,
			OutputPerMTok:       5.00,
			CacheWritePerMTok:   1.25,
			CacheWrite1hPerMTok: 2.00,
			CacheReadPerMTok:    0.10,
		},
		// Claude 3.5 Haiku ($0.80 / $4.00)
		"claude-3-5-haiku": {
			InputPerMTok:        0.80,
			OutputPerMTok:       4.00,
			CacheWritePerMTok:   1.00,
			CacheWrite1hPerMTok: 1.60,
			CacheReadPerMTok:    0.08,
		},
		// Claude 3 Haiku ($0.25 / $1.25)
		"claude-3-haiku": {
			InputPerMTok:        0.25,
			OutputPerMTok:       1.25,
			CacheWritePerMTok:   0.30,
			CacheWrite1hPerMTok: 0.50,
			CacheReadPerMTok:    0.03,
		},
	}
}

// Default returns a Config populated with Trimbar defaults.
func Default() *Config {
	dataDir := defaultDataDir()
	return &Config{
		Host:        DefaultHost,
		Port:        DefaultPort,
		UpstreamURL: DefaultUpstreamURL,
		DataDir:     dataDir,
		DBPath:      filepath.Join(dataDir, DefaultDBFileName),
		Pricing:     DefaultPricingTable(),
	}
}

// Load returns a Config initialized with defaults, overlaid with optional
// ~/.trimbar/config.toml (if present) and TRIMBAR_* environment variables.
func Load() (*Config, error) {
	cfg := Default()

	if envDir := strings.TrimSpace(os.Getenv("TRIMBAR_DATA_DIR")); envDir != "" {
		cfg.DataDir = envDir
		cfg.DBPath = filepath.Join(envDir, DefaultDBFileName)
	}

	configPath := filepath.Join(cfg.DataDir, DefaultConfigFile)
	if err := cfg.loadTOMLFile(configPath); err != nil {
		return nil, err
	}

	if host := strings.TrimSpace(os.Getenv("TRIMBAR_HOST")); host != "" {
		cfg.Host = host
	}
	if portStr := strings.TrimSpace(os.Getenv("TRIMBAR_PORT")); portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil || p <= 0 || p > 65535 {
			return nil, fmt.Errorf("invalid TRIMBAR_PORT %q", portStr)
		}
		cfg.Port = p
	}
	if upstream := strings.TrimSpace(os.Getenv("TRIMBAR_UPSTREAM_URL")); upstream != "" {
		cfg.UpstreamURL = strings.TrimRight(upstream, "/")
	}
	if trimEnv := strings.TrimSpace(os.Getenv("TRIMBAR_TRIM_ENABLED")); trimEnv != "" {
		b, err := strconv.ParseBool(trimEnv)
		if err != nil {
			return nil, fmt.Errorf("invalid TRIMBAR_TRIM_ENABLED %q", trimEnv)
		}
		cfg.TrimEnabled = b
	}

	return cfg, nil
}

// ListenAddr returns host:port formatted for net.Listen / http.Server.
func (c *Config) ListenAddr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// EnsureDataDir creates the data directory with 0700 permissions if it does not exist.
func (c *Config) EnsureDataDir() error {
	if c.DataDir == "" {
		return fmt.Errorf("data directory is empty")
	}
	return os.MkdirAll(c.DataDir, 0o700)
}

// LookupPricing resolves the pricing entry for an Anthropic model identifier
// (e.g., "claude-sonnet-4-20250514" -> "claude-sonnet-4").
// It matches exact keys first, then the longest matching prefix.
func (c *Config) LookupPricing(model string) (ModelPricing, bool) {
	if c == nil || len(c.Pricing) == 0 {
		return ModelPricing{}, false
	}
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return ModelPricing{}, false
	}

	if p, ok := c.Pricing[normalized]; ok {
		return p, true
	}

	var bestKey string
	var bestPricing ModelPricing
	for k, p := range c.Pricing {
		lk := strings.ToLower(k)
		if strings.HasPrefix(normalized, lk) {
			// Ensure boundary match (exact, hyphen, dot, or @) so "claude-opus-4" doesn't collide unexpectedly.
			if len(normalized) == len(lk) || isModelBoundary(normalized[len(lk)]) {
				if len(lk) > len(bestKey) {
					bestKey = lk
					bestPricing = p
				}
			}
		}
	}
	if bestKey != "" {
		return bestPricing, true
	}
	return ModelPricing{}, false
}

func isModelBoundary(b byte) bool {
	return b == '-' || b == '.' || b == '@' || b == ':'
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return DefaultDataDirName
	}
	return filepath.Join(home, DefaultDataDirName)
}

// loadTOMLFile reads simple top-level key = value settings from config.toml.
// Missing file is not an error.
func (c *Config) loadTOMLFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)

		switch key {
		case "host":
			if val != "" {
				c.Host = val
			}
		case "port":
			p, err := strconv.Atoi(val)
			if err != nil || p <= 0 || p > 65535 {
				return fmt.Errorf("%s:%d: invalid port %q", path, lineNum, val)
			}
			c.Port = p
		case "upstream_url":
			if val != "" {
				c.UpstreamURL = strings.TrimRight(val, "/")
			}
		case "db_path":
			if val != "" {
				c.DBPath = val
			}
		case "trim_enabled":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("%s:%d: invalid trim_enabled %q", path, lineNum, val)
			}
			c.TrimEnabled = b
		}
	}
	return scanner.Err()
}
