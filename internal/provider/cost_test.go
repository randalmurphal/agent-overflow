package provider

import (
	"math"
	"testing"
)

// almostEqual checks whether two floats are within a small tolerance.
func almostEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) < tolerance
}

func TestCalculateCost_ClaudeSonnet(t *testing.T) {
	usage := TokenUsage{
		InputTokens:  1_000_000,
		OutputTokens: 500_000,
	}
	// claude-sonnet: $3.00/M input + $15.00/M output
	// 1M input * $3.00 = $3.00; 0.5M output * $15.00 = $7.50
	want := 10.50
	got := CalculateCost("claude-sonnet-4-6", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(claude-sonnet-4-6) = %f, want %f", got, want)
	}
}

func TestCalculateCost_ClaudeFable(t *testing.T) {
	usage := TokenUsage{
		InputTokens:              1_000_000,
		OutputTokens:             500_000,
		CacheCreationInputTokens: 1_000_000,
		CacheReadInputTokens:     1_000_000,
	}
	// The runtime slug "claude-fable-5" must resolve to the
	// "claude-fable" family via matchPricing's suffix trim, and all
	// four price components must be wired (a missing entry silently
	// returns $0): $10/M in + $50/M out + $12.50/M cache write +
	// $1.00/M cache read = 10 + 25 + 12.50 + 1.00 = 48.50.
	want := 48.50
	got := CalculateCost("claude-fable-5", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(claude-fable-5) = %f, want %f", got, want)
	}
}

func TestCalculateCost_GPT54(t *testing.T) {
	usage := TokenUsage{
		InputTokens:  2_000_000,
		OutputTokens: 1_000_000,
	}
	// gpt-5.4 -> gpt-5.4 key: $2.50/M input + $15.00/M output
	// 2M input * $2.50 = $5.00; 1M output * $15.00 = $15.00
	want := 20.00
	got := CalculateCost("gpt-5.4", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(gpt-5.4) = %f, want %f", got, want)
	}
}

func TestCalculateCost_GPT55(t *testing.T) {
	usage := TokenUsage{
		InputTokens:  2_000_000,
		OutputTokens: 1_000_000,
	}
	// gpt-5.5 has its own official pricing:
	// 2M input * $5.00 = $10.00; 1M output * $30.00 = $30.00
	want := 40.00
	got := CalculateCost("gpt-5.5", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(gpt-5.5) = %f, want %f", got, want)
	}
}

func TestCalculateCost_GPT55WithCachedInput(t *testing.T) {
	usage := TokenUsage{
		InputTokens:          1_000_000,
		OutputTokens:         1_000_000,
		CacheReadInputTokens: 1_000_000,
	}
	// gpt-5.5: $5.00/M input + $30.00/M output + $0.50/M cached input
	want := 35.50
	got := CalculateCost("gpt-5.5", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(gpt-5.5 with cache read) = %f, want %f", got, want)
	}
}

func TestCalculateCost_GPT54Mini(t *testing.T) {
	usage := TokenUsage{
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	}
	// gpt-5.4-mini key: $0.75/M input + $4.50/M output
	want := 5.25
	got := CalculateCost("gpt-5.4-mini", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(gpt-5.4-mini) = %f, want %f", got, want)
	}
}

func TestCalculateCost_UnknownModel(t *testing.T) {
	usage := TokenUsage{
		InputTokens:  1_000_000,
		OutputTokens: 500_000,
	}
	got := CalculateCost("totally-unknown-model", usage)
	if got != 0 {
		t.Errorf("CalculateCost(unknown) = %f, want 0", got)
	}
}

func TestCalculateCost_WithCacheReadTokens(t *testing.T) {
	usage := TokenUsage{
		InputTokens:          500_000,
		OutputTokens:         200_000,
		CacheReadInputTokens: 1_000_000,
	}
	// claude-opus: $5.00/M input, $25.00/M output, $0.50/M cache read
	// 0.5M * $5.00 = $2.50; 0.2M * $25.00 = $5.00; 1M * $0.50 = $0.50
	want := 8.00
	got := CalculateCost("claude-opus-4-6", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(claude-opus-4-6 with cache) = %f, want %f", got, want)
	}
}

func TestCalculateCost_WithCacheCreationTokens(t *testing.T) {
	usage := TokenUsage{
		InputTokens:              500_000,
		OutputTokens:             200_000,
		CacheCreationInputTokens: 1_000_000,
	}
	// claude-sonnet: $3.00/M input, $15.00/M output, $3.75/M cache creation
	// 0.5M * $3.00 = $1.50; 0.2M * $15.00 = $3.00; 1M * $3.75 = $3.75
	want := 8.25
	got := CalculateCost("claude-sonnet-4-6", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(claude-sonnet-4-6 with cache creation) = %f, want %f", got, want)
	}
}

func TestCalculateCost_CacheCreationIgnoredForModelsWithoutPricing(t *testing.T) {
	usage := TokenUsage{
		CacheCreationInputTokens: 1_000_000,
	}
	got := CalculateCost("gpt-5.4", usage)
	if got != 0 {
		t.Errorf("CalculateCost(gpt-5.4 cache creation only) = %f, want 0", got)
	}
}

func TestCalculateCost_ZeroTokens(t *testing.T) {
	usage := TokenUsage{}
	got := CalculateCost("claude-sonnet-4-6", usage)
	if got != 0 {
		t.Errorf("CalculateCost(zero tokens) = %f, want 0", got)
	}
}

func TestCalculateCost_O3(t *testing.T) {
	usage := TokenUsage{
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	}
	// o3: $1.75/M input + $14.00/M output
	want := 15.75
	got := CalculateCost("o3", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(o3) = %f, want %f", got, want)
	}
}

func TestCalculateCost_O4Mini(t *testing.T) {
	usage := TokenUsage{
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	}
	// o4-mini: $0.25/M input + $2.00/M output
	want := 2.25
	got := CalculateCost("o4-mini", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(o4-mini) = %f, want %f", got, want)
	}
}

func TestCalculateCost_HaikuWithCacheRead(t *testing.T) {
	usage := TokenUsage{
		InputTokens:          100_000,
		OutputTokens:         50_000,
		CacheReadInputTokens: 500_000,
	}
	// claude-haiku: $1.00/M input, $5.00/M output, $0.10/M cache read
	// 0.1M * $1.00 = $0.10; 0.05M * $5.00 = $0.25; 0.5M * $0.10 = $0.05
	want := 0.40
	got := CalculateCost("claude-haiku-4-5", usage)
	if !almostEqual(got, want, 0.001) {
		t.Errorf("CalculateCost(claude-haiku-4-5 with cache) = %f, want %f", got, want)
	}
}

func TestMatchPricing_ExactMatch(t *testing.T) {
	p, ok := matchPricing("o3")
	if !ok {
		t.Fatal("expected match for exact key o3")
	}
	if p.InputPerMToken != 1.75 {
		t.Errorf("InputPerMToken = %f, want 1.75", p.InputPerMToken)
	}
}

func TestMatchPricing_PrefixMatch(t *testing.T) {
	p, ok := matchPricing("claude-opus-4-6")
	if !ok {
		t.Fatal("expected match for claude-opus-4-6 via prefix")
	}
	if p.InputPerMToken != 5.00 {
		t.Errorf("InputPerMToken = %f, want 5.00", p.InputPerMToken)
	}
}

func TestMatchPricing_DottedVersionFallback(t *testing.T) {
	p, ok := matchPricing("gpt-5.6")
	if !ok {
		t.Fatal("expected match for gpt-5.6 via dotted fallback")
	}
	if p.InputPerMToken != 1.25 {
		t.Errorf("InputPerMToken = %f, want 1.25", p.InputPerMToken)
	}
}

func TestMatchPricing_NoMatch(t *testing.T) {
	_, ok := matchPricing("gemini-pro")
	if ok {
		t.Error("expected no match for gemini-pro")
	}
}
