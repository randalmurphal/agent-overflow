package usagecost

import (
	"math"
	"testing"
)

// almostEqual checks whether two floats are within a small tolerance.
func almostEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) < tolerance
}

// TestPrice_AllFamiliesInputOutputOnly hand-computes input+output cost
// (no cache) for every rate-table family at exactly 1M tokens each, so
// each expected value is just Input+Output per the table above. This is
// the arithmetic spot check across the whole table, including exact
// entries (claude-sonnet-5, gpt-5.2-codex, gpt-5.1-codex, gpt-5-codex)
// and family fallbacks (claude-sonnet, gpt-5, gpt-5.4, gpt-5.4-mini).
func TestPrice_AllFamiliesInputOutputOnly(t *testing.T) {
	cases := []struct {
		model       string
		wantCostUSD float64
	}{
		{"claude-fable-5", 60.00},    // 10 + 50
		{"claude-opus-4-6", 30.00},   // 5 + 25
		{"claude-sonnet-5", 12.00},   // 2 + 10 (intro, exact)
		{"claude-sonnet-4-6", 18.00}, // 3 + 15 (family)
		{"claude-haiku-4-5", 6.00},   // 1 + 5
		{"gpt-5.2-codex", 15.75},     // 1.75 + 14
		{"gpt-5.1-codex", 15.75},
		{"gpt-5-codex", 15.75},
		{"gpt-5.5", 35.00},     // 5 + 30
		{"gpt-5.4", 17.50},     // 2.5 + 15
		{"gpt-5.4-mini", 5.25}, // 0.75 + 4.5
		{"gpt-5", 11.25},       // 1.25 + 10
		{"o3", 15.75},          // 1.75 + 14
		{"o4-mini", 2.25},      // 0.25 + 2
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			got, ok := Price(c.model, 1_000_000, 1_000_000, 0, 0)
			if !ok {
				t.Fatalf("Price(%q) ok = false, want true", c.model)
			}
			if !almostEqual(got, c.wantCostUSD, 0.001) {
				t.Errorf("Price(%q) = %f, want %f", c.model, got, c.wantCostUSD)
			}
		})
	}
}

// TestPrice_ClaudeHaikuFamilyMatch exercises the progressive
// suffix-trim path with all four token components: "claude-haiku-4-5"
// must resolve to the "claude-haiku" family.
func TestPrice_ClaudeHaikuFamilyMatch(t *testing.T) {
	// claude-haiku: $1/M in, $5/M out, $0.10/M cache read, $2/M cache write.
	// 2M*$1 + 1M*$5 + 0.5M*$0.10 + 0.1M*$2 = 2 + 5 + 0.05 + 0.2 = 7.25
	want := 7.25
	got, ok := Price("claude-haiku-4-5", 2_000_000, 1_000_000, 500_000, 100_000)
	if !ok {
		t.Fatal("Price(claude-haiku-4-5) ok = false, want true")
	}
	if !almostEqual(got, want, 0.001) {
		t.Errorf("Price(claude-haiku-4-5) = %f, want %f", got, want)
	}
}

// TestPrice_ContextTierSuffixStripped confirms the "[1m]" context-tier
// marker is stripped before matching, and that the stripped slug still
// resolves to its EXACT entry (claude-sonnet-5, not the claude-sonnet
// family) rather than losing precision from the strip.
func TestPrice_ContextTierSuffixStripped(t *testing.T) {
	// claude-sonnet-5 (exact): $2/M in, $10/M out, $0.20/M cache read,
	// $4/M cache write.
	// 1M*$2 + 0.5M*$10 + 1M*$0.20 + 1M*$4 = 2 + 5 + 0.20 + 4 = 11.20
	want := 11.20
	got, ok := Price("claude-sonnet-5[1m]", 1_000_000, 500_000, 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("Price(claude-sonnet-5[1m]) ok = false, want true")
	}
	if !almostEqual(got, want, 0.001) {
		t.Errorf("Price(claude-sonnet-5[1m]) = %f, want %f", got, want)
	}
}

// TestPrice_ExactOverFamilyPrecedence proves the intro claude-sonnet-5
// entry wins over the claude-sonnet family price for the exact slug,
// while a versioned slug that isn't the exact intro string still falls
// through to the family rate.
func TestPrice_ExactOverFamilyPrecedence(t *testing.T) {
	usage := struct{ input, output int64 }{1_000_000, 500_000}

	exact, ok := Price("claude-sonnet-5", usage.input, usage.output, 0, 0)
	if !ok {
		t.Fatal("Price(claude-sonnet-5) ok = false, want true")
	}
	if want := 7.00; !almostEqual(exact, want, 0.001) { // 1*2 + 0.5*10
		t.Errorf("Price(claude-sonnet-5) = %f, want %f", exact, want)
	}

	family, ok := Price("claude-sonnet-4-6", usage.input, usage.output, 0, 0)
	if !ok {
		t.Fatal("Price(claude-sonnet-4-6) ok = false, want true")
	}
	if want := 10.50; !almostEqual(family, want, 0.001) { // 1*3 + 0.5*15
		t.Errorf("Price(claude-sonnet-4-6) = %f, want %f", family, want)
	}

	if almostEqual(exact, family, 0.001) {
		t.Fatalf("exact intro price (%f) must differ from family price (%f)", exact, family)
	}
}

// TestPrice_UnknownModelReturnsNotOK confirms callers can distinguish
// "no known pricing" from "priced at $0".
func TestPrice_UnknownModelReturnsNotOK(t *testing.T) {
	got, ok := Price("totally-unknown-model", 1_000_000, 500_000, 0, 0)
	if ok {
		t.Fatalf("Price(unknown) ok = true, want false")
	}
	if got != 0 {
		t.Errorf("Price(unknown) cost = %f, want 0", got)
	}
}

// TestPrice_ZeroUsageKnownModelStillOK confirms a known model with zero
// tokens is priced ($0) rather than reported as unknown.
func TestPrice_ZeroUsageKnownModelStillOK(t *testing.T) {
	got, ok := Price("claude-opus-4-6", 0, 0, 0, 0)
	if !ok {
		t.Fatal("Price(claude-opus-4-6, zero usage) ok = false, want true")
	}
	if got != 0 {
		t.Errorf("Price(claude-opus-4-6, zero usage) = %f, want 0", got)
	}
}

// TestPrice_OpenAICacheWriteIsFree confirms OpenAI cache-write tokens
// never contribute cost, since OpenAI does not bill cache writes.
func TestPrice_OpenAICacheWriteIsFree(t *testing.T) {
	withoutCacheWrite, ok := Price("gpt-5.4", 1_000_000, 1_000_000, 0, 0)
	if !ok {
		t.Fatal("Price(gpt-5.4) ok = false, want true")
	}
	withCacheWrite, ok := Price("gpt-5.4", 1_000_000, 1_000_000, 0, 5_000_000)
	if !ok {
		t.Fatal("Price(gpt-5.4, with cache write) ok = false, want true")
	}
	if !almostEqual(withoutCacheWrite, withCacheWrite, 0.001) {
		t.Errorf("cache-write tokens changed cost: without=%f with=%f", withoutCacheWrite, withCacheWrite)
	}
}

// TestPrice_DottedCodexVersionMissesFamilyFallback documents (and
// regression-guards) the trim-algorithm quirk called out in the
// package comment: a hypothetical "gpt-5.3-codex" without its own
// explicit entry does NOT fall back to "gpt-5-codex" pricing. It trims
// to "gpt-5.3" then "gpt-5", landing on the plain (non-codex) family
// rate instead.
func TestPrice_DottedCodexVersionMissesFamilyFallback(t *testing.T) {
	// gpt-5 family: $1.25/M in + $10/M out = 1*1.25 + 1*10 = 11.25.
	// (NOT gpt-5-codex's 1.75+14=15.75, which the naive reading of
	// "family fallback" might expect.)
	want := 11.25
	got, ok := Price("gpt-5.3-codex", 1_000_000, 1_000_000, 0, 0)
	if !ok {
		t.Fatal("Price(gpt-5.3-codex) ok = false, want true (falls back to gpt-5)")
	}
	if !almostEqual(got, want, 0.001) {
		t.Errorf("Price(gpt-5.3-codex) = %f, want %f (gpt-5 fallback, not gpt-5-codex)", got, want)
	}
}
