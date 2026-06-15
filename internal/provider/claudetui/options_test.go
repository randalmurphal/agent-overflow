package claudetui

import (
	"testing"

	"agent-overflow/internal/provider"
)

// TestConfigFromOptionsCarriesEffort pins that the TUI launch Config carries the
// reasoning effort selected in AO. claudetui reuses claude.ConfigFromOptions for
// the SessionOptions→CLI-value mapping, so xhigh resolves to the native "xhigh"
// the 2.1.170 --effort flag accepts (not the old "max" collapse), and an unset
// effort stays empty so launch omits the flag. Before the fix this field was
// dropped entirely and the TUI ran at its own default tier.
func TestConfigFromOptionsCarriesEffort(t *testing.T) {
	cases := []struct {
		effort provider.ReasoningEffort
		want   string
	}{
		{provider.EffortLow, "low"},
		{provider.EffortHigh, "high"},
		{provider.EffortXHigh, "xhigh"},
		{provider.EffortMax, "max"},
		{provider.EffortNone, ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.effort), func(t *testing.T) {
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider:        "claude-tui",
				Model:           "claude-opus-4-8",
				ReasoningEffort: tc.effort,
			})
			if cfg.ReasoningEffort != tc.want {
				t.Fatalf("ReasoningEffort = %q, want %q", cfg.ReasoningEffort, tc.want)
			}
		})
	}
}
