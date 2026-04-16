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
	// "gpt-5" also serves as a prefix fallback for any gpt-5-* variant via
	// matchPricing's progressive prefix stripping (e.g. "gpt-5-turbo" would
	// match here after failing exact lookup).
	"gpt-5": {
		InputPerMToken:     1.75,
		OutputPerMToken:    14.00,
		CacheReadPerMToken: 0.175,
	},
	"gpt-5.4": {
		InputPerMToken:     1.75,
		OutputPerMToken:    14.00,
		CacheReadPerMToken: 0.175,
	},
	"gpt-5.4-mini": {
		InputPerMToken:     0.25,
		OutputPerMToken:    2.00,
		CacheReadPerMToken: 0.025,
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
// then progressively stripping trailing "-segment" pieces until a prefix matches.
// Example: "claude-sonnet-4-6" tries "claude-sonnet-4-6", "claude-sonnet-4", "claude-sonnet".
func matchPricing(model string) (ModelPricing, bool) {
	// Try exact match first.
	if p, ok := KnownPricing[model]; ok {
		return p, true
	}

	// Progressive prefix matching: strip trailing segments split on "-".
	parts := strings.Split(model, "-")
	for i := len(parts) - 1; i >= 1; i-- {
		prefix := strings.Join(parts[:i], "-")
		if p, ok := KnownPricing[prefix]; ok {
			return p, true
		}
	}

	return ModelPricing{}, false
}
