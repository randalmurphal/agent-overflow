package main

import (
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// internal/settings cannot import internal/provider (cycle), so its
// per-provider reasoning-effort tables are copies of
// provider.ReasoningEffortsForProvider. This is the cross-check that keeps them
// honest in both directions.
//
// The failure it prevents is asymmetric and both halves are silent. A tier the
// provider package offers but settings rejects makes the settings UI refuse a
// value the composer's own picker hands out — Codex's max and ultra are exactly
// that pair, added to the provider enum and to the store CHECK (migration v19)
// before this table was widened. A tier settings accepts but no provider does
// would validate, persist, and then be coerced away at spawn, so the user's
// configured effort silently is not the one that runs.
func TestTextGenerationEffortsMatchTheProviderSets(t *testing.T) {
	// Text generation is gated to these two; claude-tui is never routed here.
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		t.Run(providerName, func(t *testing.T) {
			canonical := provider.ReasoningEffortsForProvider(providerName)
			allowed := settings.AllowedTextGenerationEfforts(providerName)

			for _, effort := range canonical {
				if !slices.Contains(allowed, string(effort)) {
					t.Errorf("%s offers effort %q but settings rejects it for text generation", providerName, effort)
				}
			}
			for _, effort := range allowed {
				if !slices.Contains(canonical, provider.ReasoningEffort(effort)) {
					t.Errorf("settings accepts %q for %s but the provider does not offer it; the value would be coerced away at spawn", effort, providerName)
				}
			}

			// Every accepted slug survives a real validation call, not merely
			// a map lookup, and every one is named in the message a rejected
			// value produces — the list the user is shown must not be shorter
			// than the list that works.
			for _, effort := range canonical {
				if err := settings.ValidateTextGenerationReasoningEffort(providerName, string(effort)); err != nil {
					t.Errorf("settings rejected %s/%s: %v", providerName, effort, err)
				}
			}
			err := settings.ValidateTextGenerationReasoningEffort(providerName, "ultranope")
			if err == nil {
				t.Fatalf("settings accepted an unknown effort for %s", providerName)
			}
			for _, effort := range canonical {
				if !strings.Contains(err.Error(), string(effort)) {
					t.Errorf("the rejection message for %s omits the legal tier %q: %s", providerName, effort, err)
				}
			}
		})
	}
}
