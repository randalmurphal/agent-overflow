package provider

import (
	"math"
	"testing"
)

// TestComputeContextPercent_CodexFormula mirrors the canonical Codex
// formula in codex-rs/protocol/src/protocol.rs:percent_of_context_window_remaining
// (BASELINE_TOKENS = 12000). We display percent USED, not percent
// remaining — values below are converted (`100 - percent_left`).
func TestComputeContextPercent_CodexFormula(t *testing.T) {
	type tc struct {
		name string
		used int
		max  int
		want float64
	}
	cases := []tc{
		{
			name: "zero used → 0%",
			used: 0,
			max:  200000,
			want: 0,
		},
		{
			name: "below baseline used → still 0% (baseline is the floor)",
			used: 5000,
			max:  200000,
			want: 0,
		},
		{
			name: "126 used / 258400 max → effectively 0% (Codex parity)",
			used: 126,
			max:  258400,
			want: 0,
		},
		{
			name: "exactly at baseline → 0%",
			used: 12000,
			max:  200000,
			want: 0,
		},
		{
			name: "100k used / 200k max → ~46.8% (Codex parity)",
			used: 100000,
			max:  200000,
			// (100000-12000) / (200000-12000) = 88000 / 188000 ≈ 46.808510...
			want: float64(88000) / float64(188000) * 100,
		},
		{
			name: "max usage → 100%",
			used: 200000,
			max:  200000,
			want: 100,
		},
		{
			name: "above max → clamped to 100%",
			used: 250000,
			max:  200000,
			want: 100,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeContextPercent(Codex, c.used, c.max)
			if math.Abs(got-c.want) > 1e-9 {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestComputeContextPercent_ClaudeIsPlainRatio pins that non-Codex
// providers keep the simple `used / max * 100` formula.
func TestComputeContextPercent_ClaudeIsPlainRatio(t *testing.T) {
	got := ComputeContextPercent(Claude, 100000, 200000)
	if got != 50 {
		t.Fatalf("Claude plain ratio: got %v, want 50", got)
	}
}

// TestComputeContextPercent_EdgeWindow guards against
// codexBaselineTokens >= max (e.g. tiny custom windows). Codex returns
// 100% in that case, mirroring the source check
// `if context_window <= BASELINE_TOKENS { return 0; }` (which means
// "0% remaining" → "100% used" in our convention).
func TestComputeContextPercent_EdgeWindow(t *testing.T) {
	if got := ComputeContextPercent(Codex, 0, 12000); got != 100 {
		t.Fatalf("Codex max==baseline: got %v, want 100", got)
	}
	if got := ComputeContextPercent(Codex, 0, 0); got != 0 {
		t.Fatalf("Codex max==0: got %v, want 0", got)
	}
	if got := ComputeContextPercent(Claude, 100, 0); got != 0 {
		t.Fatalf("Claude max==0: got %v, want 0", got)
	}
}
