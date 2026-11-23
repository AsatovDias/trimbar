package pricing

import (
	"github.com/trimbar/trimbar/internal/config"
)

const tokensPerMillion = 1_000_000.0

// Usage represents Anthropic token usage counts for a single API turn.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	// Optional split when Anthropic reports TTL-specific cache creation counts.
	CacheCreation5mTokens int64
	CacheCreation1hTokens int64
}

// TotalInputTokens returns all prompt tokens processed (uncached + cache write + cache read).
func (u Usage) TotalInputTokens() int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// CacheHitRatio returns the fraction of input tokens served from prompt cache in [0, 1].
func (u Usage) CacheHitRatio() float64 {
	total := u.TotalInputTokens()
	if total <= 0 {
		return 0
	}
	return float64(u.CacheReadInputTokens) / float64(total)
}

// CostBreakdown holds exact Anthropic USD costs and prompt-cache savings for a request.
type CostBreakdown struct {
	Model              string
	PricingKnown       bool
	InputCostUSD       float64
	OutputCostUSD      float64
	CacheWriteCostUSD  float64
	CacheReadCostUSD   float64
	TotalCostUSD       float64
	CacheSavingsUSD    float64 // Gross savings from cache reads vs uncached input rate
	NetCacheSavingsUSD float64 // CacheSavingsUSD minus the 0.25x/1.0x cache-write surcharge
}

// ShadowSavings represents an honest estimate of USD recoverable by eliminating
// wasted prompt tokens (e.g., duplicate Read results or ANSI escape codes).
// Rather than naively pricing all wasted tokens at full uncached input rates,
// it reports the honest blended rate reflecting actual prompt cache usage.
type ShadowSavings struct {
	Model                string
	PricingKnown         bool
	WastedTokens         int64
	EffectiveInputPerMTok float64 // Blended $/1M rate paid on this turn's prompt tokens
	HonestSavingsUSD     float64 // WastedTokens priced at EffectiveInputPerMTok
	UncachedSavingsUSD   float64 // Upper bound if tokens were uncached input
	CacheWriteSavingsUSD float64 // Savings if avoided on a cache-creation turn
	CacheReadSavingsUSD  float64 // Per-turn savings when waste sits inside cached prefix
}

// Calculator computes Anthropic USD costs and honest shadow savings.
type Calculator struct {
	cfg *config.Config
}

// NewCalculator creates a Calculator backed by cfg's pricing table.
// If cfg is nil, config.Default() is used.
func NewCalculator(cfg *config.Config) *Calculator {
	if cfg == nil {
		cfg = config.Default()
	}
	return &Calculator{cfg: cfg}
}

// CalculateCost computes the exact USD cost breakdown for a single Anthropic turn.
func (c *Calculator) CalculateCost(model string, usage Usage) CostBreakdown {
	p, ok := c.cfg.LookupPricing(model)
	if !ok {
		return CostBreakdown{
			Model:        model,
			PricingKnown: false,
		}
	}

	inputCost := float64(usage.InputTokens) * p.InputPerMTok / tokensPerMillion
	outputCost := float64(usage.OutputTokens) * p.OutputPerMTok / tokensPerMillion
	cacheReadCost := float64(usage.CacheReadInputTokens) * p.CacheReadPerMTok / tokensPerMillion

	var cacheWriteCost float64
	var cacheWriteSurcharge float64
	if usage.CacheCreation5mTokens > 0 || usage.CacheCreation1hTokens > 0 {
		rate1h := p.CacheWrite1hPerMTok
		if rate1h == 0 {
			rate1h = p.InputPerMTok * 2.0
		}
		write5m := float64(usage.CacheCreation5mTokens) * p.CacheWritePerMTok / tokensPerMillion
		write1h := float64(usage.CacheCreation1hTokens) * rate1h / tokensPerMillion
		// Account for any remaining cache_creation_input_tokens not covered by the split.
		unaccounted := usage.CacheCreationInputTokens - (usage.CacheCreation5mTokens + usage.CacheCreation1hTokens)
		var writeRem float64
		if unaccounted > 0 {
			writeRem = float64(unaccounted) * p.CacheWritePerMTok / tokensPerMillion
		}
		cacheWriteCost = write5m + write1h + writeRem
		cacheWriteSurcharge = float64(usage.CacheCreation5mTokens)*(p.CacheWritePerMTok-p.InputPerMTok)/tokensPerMillion +
			float64(usage.CacheCreation1hTokens)*(rate1h-p.InputPerMTok)/tokensPerMillion
		if unaccounted > 0 {
			cacheWriteSurcharge += float64(unaccounted) * (p.CacheWritePerMTok - p.InputPerMTok) / tokensPerMillion
		}
	} else {
		cacheWriteCost = float64(usage.CacheCreationInputTokens) * p.CacheWritePerMTok / tokensPerMillion
		cacheWriteSurcharge = float64(usage.CacheCreationInputTokens) * (p.CacheWritePerMTok - p.InputPerMTok) / tokensPerMillion
	}

	cacheSavings := float64(usage.CacheReadInputTokens) * (p.InputPerMTok - p.CacheReadPerMTok) / tokensPerMillion
	netCacheSavings := cacheSavings - cacheWriteSurcharge

	return CostBreakdown{
		Model:              model,
		PricingKnown:       true,
		InputCostUSD:       inputCost,
		OutputCostUSD:      outputCost,
		CacheWriteCostUSD:  cacheWriteCost,
		CacheReadCostUSD:   cacheReadCost,
		TotalCostUSD:       inputCost + outputCost + cacheWriteCost + cacheReadCost,
		CacheSavingsUSD:    cacheSavings,
		NetCacheSavingsUSD: netCacheSavings,
	}
}

// CalculateShadowSavings computes honest potential USD savings from eliminating wastedTokens.
// When usage contains prompt tokens, the honest savings rate is the turn's actual blended
// input cost per 1M tokens (weighted across uncached, cache write, and cache read),
// avoiding inflated savings estimates on heavily cached sessions.
func (c *Calculator) CalculateShadowSavings(model string, usage Usage, wastedTokens int64) ShadowSavings {
	if wastedTokens <= 0 {
		return ShadowSavings{Model: model}
	}
	p, ok := c.cfg.LookupPricing(model)
	if !ok {
		return ShadowSavings{
			Model:        model,
			PricingKnown: false,
			WastedTokens: wastedTokens,
		}
	}

	cost := c.CalculateCost(model, usage)
	totalInput := usage.TotalInputTokens()

	effectivePerMTok := p.InputPerMTok
	if totalInput > 0 {
		totalPromptCost := cost.InputCostUSD + cost.CacheWriteCostUSD + cost.CacheReadCostUSD
		effectivePerMTok = (totalPromptCost / float64(totalInput)) * tokensPerMillion
	}

	return ShadowSavings{
		Model:                 model,
		PricingKnown:          true,
		WastedTokens:          wastedTokens,
		EffectiveInputPerMTok: effectivePerMTok,
		HonestSavingsUSD:      float64(wastedTokens) * effectivePerMTok / tokensPerMillion,
		UncachedSavingsUSD:    float64(wastedTokens) * p.InputPerMTok / tokensPerMillion,
		CacheWriteSavingsUSD:  float64(wastedTokens) * p.CacheWritePerMTok / tokensPerMillion,
		CacheReadSavingsUSD:   float64(wastedTokens) * p.CacheReadPerMTok / tokensPerMillion,
	}
}
