package provider

import "strings"

// ModelPricing holds per-million-token pricing for a model family.
type ModelPricing struct {
	InputPerMToken         float64
	OutputPerMToken        float64
	CacheCreationPerMToken float64
	CacheReadPerMToken     float64
}

// KnownPricing maps model slug prefixes to pricing.
// Uses family-level matching: "claude-opus" matches any claude-opus-* slug.
var KnownPricing = map[string]ModelPricing{
	"claude-fable": {
		InputPerMToken:         10.00,
		OutputPerMToken:        50.00,
		CacheCreationPerMToken: 12.50,
		CacheReadPerMToken:     1.00,
	},
	"claude-opus": {
		InputPerMToken:         5.00,
		OutputPerMToken:        25.00,
		CacheCreationPerMToken: 6.25,
		CacheReadPerMToken:     0.50,
	},
	"claude-sonnet": {
		InputPerMToken:         3.00,
		OutputPerMToken:        15.00,
		CacheCreationPerMToken: 3.75,
		CacheReadPerMToken:     0.30,
	},
	"claude-haiku": {
		InputPerMToken:         1.00,
		OutputPerMToken:        5.00,
		CacheCreationPerMToken: 1.25,
		CacheReadPerMToken:     0.10,
	},
	// "gpt-5" serves as the family-level fallback for GPT-5 variants that
	// do not have an explicit per-version entry yet (for example a future
	// gpt-5.6 until/if we add distinct pricing).
	"gpt-5": {
		InputPerMToken:     1.25,
		OutputPerMToken:    10.00,
		CacheReadPerMToken: 0.125,
	},
	"gpt-5.5": {
		InputPerMToken:     5.00,
		OutputPerMToken:    30.00,
		CacheReadPerMToken: 0.50,
	},
	"gpt-5.4": {
		InputPerMToken:     2.50,
		OutputPerMToken:    15.00,
		CacheReadPerMToken: 0.25,
	},
	"gpt-5.4-mini": {
		InputPerMToken:     0.75,
		OutputPerMToken:    4.50,
		CacheReadPerMToken: 0.075,
	},
	"o3": {
		InputPerMToken:     1.75,
		OutputPerMToken:    14.00,
		CacheReadPerMToken: 0.175,
	},
	"o4-mini": {
		InputPerMToken:     0.25,
		OutputPerMToken:    2.00,
		CacheReadPerMToken: 0.025,
	},
}

// CalculateCost computes the USD cost for a model's token usage.
// Returns 0 for unknown models or zero usage.
func CalculateCost(model string, usage TokenUsage) float64 {
	pricing, ok := matchPricing(model)
	if !ok {
		return 0
	}

	const million = 1_000_000.0
	inputCost := float64(usage.InputTokens) / million * pricing.InputPerMToken
	outputCost := float64(usage.OutputTokens) / million * pricing.OutputPerMToken
	cacheCreationCost := float64(usage.CacheCreationInputTokens) / million * pricing.CacheCreationPerMToken
	cacheReadCost := float64(usage.CacheReadInputTokens) / million * pricing.CacheReadPerMToken

	return inputCost + outputCost + cacheCreationCost + cacheReadCost
}

// matchPricing finds pricing for a model slug by trying exact match first,
// then progressively stripping trailing family/version suffixes until a
// prefix matches. We trim the rightmost "-" or "." segment each round so
// both hyphen families (claude-sonnet-4-6 -> claude-sonnet) and dotted GPT
// versions (gpt-5.6 -> gpt-5) resolve to their family pricing.
func matchPricing(model string) (ModelPricing, bool) {
	// Try exact match first.
	if p, ok := KnownPricing[model]; ok {
		return p, true
	}

	// Progressive prefix matching: trim the trailing "-" or "." segment
	// each round until a known family prefix matches.
	candidate := model
	for {
		cut := strings.LastIndexAny(candidate, "-.")
		if cut <= 0 {
			break
		}
		candidate = candidate[:cut]
		if p, ok := KnownPricing[candidate]; ok {
			return p, true
		}
	}

	return ModelPricing{}, false
}
