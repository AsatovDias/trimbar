package pricing

import (
	"math"
	"testing"

	"github.com/AsatovDias/trimbar/internal/config"
)

func almostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestCalculateCostAndCacheSavings(t *testing.T) {
	calc := NewCalculator(config.Default())

	// Sonnet 4: Input $3, Output $15, CacheWrite $3.75, CacheRead $0.30
	usage := Usage{
		InputTokens:              10_000,  // $0.03
		OutputTokens:             2_000,   // $0.03
		CacheCreationInputTokens: 20_000,  // $0.075 (surcharge = $0.015)
		CacheReadInputTokens:     100_000, // $0.03  (gross savings vs $0.30 = $0.27)
	}

	got := calc.CalculateCost("claude-sonnet-4-20250514", usage)
	if !got.PricingKnown {
		t.Fatalf("expected PricingKnown=true")
	}
	if !almostEqual(got.InputCostUSD, 0.03, 1e-9) {
		t.Errorf("InputCostUSD = %v, want 0.03", got.InputCostUSD)
	}
	if !almostEqual(got.OutputCostUSD, 0.03, 1e-9) {
		t.Errorf("OutputCostUSD = %v, want 0.03", got.OutputCostUSD)
	}
	if !almostEqual(got.CacheWriteCostUSD, 0.075, 1e-9) {
		t.Errorf("CacheWriteCostUSD = %v, want 0.075", got.CacheWriteCostUSD)
	}
	if !almostEqual(got.CacheReadCostUSD, 0.03, 1e-9) {
		t.Errorf("CacheReadCostUSD = %v, want 0.03", got.CacheReadCostUSD)
	}
	if !almostEqual(got.TotalCostUSD, 0.165, 1e-9) {
		t.Errorf("TotalCostUSD = %v, want 0.165", got.TotalCostUSD)
	}
	if !almostEqual(got.CacheSavingsUSD, 0.27, 1e-9) {
		t.Errorf("CacheSavingsUSD = %v, want 0.27", got.CacheSavingsUSD)
	}
	if !almostEqual(got.NetCacheSavingsUSD, 0.255, 1e-9) {
		t.Errorf("NetCacheSavingsUSD = %v, want 0.255", got.NetCacheSavingsUSD)
	}
}

func TestCalculateCostWithExtended1hCacheTTL(t *testing.T) {
	calc := NewCalculator(config.Default())

	// Sonnet 4: 5m write = $3.75/MTok, 1h write = $6.00/MTok
	usage := Usage{
		CacheCreationInputTokens: 30_000,
		CacheCreation5mTokens:    10_000, // $0.0375
		CacheCreation1hTokens:    20_000, // $0.12
	}

	got := calc.CalculateCost("claude-sonnet-4-20250514", usage)
	if !almostEqual(got.CacheWriteCostUSD, 0.1575, 1e-9) {
		t.Errorf("CacheWriteCostUSD = %v, want 0.1575", got.CacheWriteCostUSD)
	}
}

func TestHonestShadowSavings(t *testing.T) {
	calc := NewCalculator(config.Default())

	// Heavily cached session: 90k cache read ($0.027) + 10k uncached ($0.03) = $0.057 over 100k tokens
	// Effective prompt rate = $0.57 / 1M tokens (much lower than naive $3.00 / 1M!)
	usage := Usage{
		InputTokens:          10_000,
		CacheReadInputTokens: 90_000,
	}

	shadow := calc.CalculateShadowSavings("claude-sonnet-4-20250514", usage, 10_000)
	if !shadow.PricingKnown {
		t.Fatalf("expected PricingKnown=true")
	}
	if !almostEqual(shadow.EffectiveInputPerMTok, 0.57, 1e-9) {
		t.Errorf("EffectiveInputPerMTok = %v, want 0.57", shadow.EffectiveInputPerMTok)
	}
	if !almostEqual(shadow.HonestSavingsUSD, 0.0057, 1e-9) {
		t.Errorf("HonestSavingsUSD = %v, want 0.0057", shadow.HonestSavingsUSD)
	}
	if !almostEqual(shadow.UncachedSavingsUSD, 0.03, 1e-9) {
		t.Errorf("UncachedSavingsUSD = %v, want 0.03", shadow.UncachedSavingsUSD)
	}
	if shadow.HonestSavingsUSD >= shadow.UncachedSavingsUSD {
		t.Errorf("expected HonestSavingsUSD (%v) < UncachedSavingsUSD (%v) on cached turn",
			shadow.HonestSavingsUSD, shadow.UncachedSavingsUSD)
	}
}

func TestFallbackSonnetPricingForUnknownModel(t *testing.T) {
	calc := NewCalculator(config.Default())

	usage := Usage{
		InputTokens:          10_000, // $0.03 at Sonnet fallback ($3/MTok)
		OutputTokens:         1_000,  // $0.015 at Sonnet fallback ($15/MTok)
		CacheReadInputTokens: 10_000, // $0.003 at Sonnet fallback ($0.30/MTok)
	}

	got := calc.CalculateCost("claude-future-9-20290101", usage)
	if got.PricingKnown {
		t.Errorf("expected PricingKnown=false for unknown future model")
	}
	if !got.Estimated {
		t.Errorf("expected Estimated=true for fallback Sonnet pricing")
	}
	if !almostEqual(got.TotalCostUSD, 0.048, 1e-9) {
		t.Errorf("fallback TotalCostUSD = %v, want 0.048", got.TotalCostUSD)
	}

	shadow := calc.CalculateShadowSavings("claude-future-9-20290101", usage, 1_000)
	if !shadow.Estimated || shadow.HonestSavingsUSD <= 0 {
		t.Errorf("expected Estimated=true and HonestSavingsUSD>0, got %+v", shadow)
	}

	trimSave := calc.CalculateTrimmedSavings("claude-future-9-20290101", 10_000)
	if !almostEqual(trimSave, 0.03, 1e-9) {
		t.Errorf("fallback CalculateTrimmedSavings = %v, want 0.03", trimSave)
	}
}
