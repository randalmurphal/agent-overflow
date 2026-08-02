package claudetui

import (
	"reflect"
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

// TestConfigFromOptionsIgnoresEveryRuntimeMode is the behavioural proof behind
// Capabilities.EnforcesRuntimeMode == false for claude-tui: approvals live
// inside the real TUI, so no runtime mode may reach the launch Config. Every
// tier must produce byte-identical output — including auto, whose Claude
// mapping is a `--permission-mode` flag the headless Config carries and this
// one deliberately drops.
//
// Iterating provider.AllRuntimeModes rather than naming tiers is the point: a
// fifth tier must be inert here by construction, not by someone remembering to
// extend a list. The failure this catches is the reverse of the obvious one —
// not "the mode was dropped" but "a new mode quietly started being honoured on
// a provider whose approvals AO does not drive".
func TestConfigFromOptionsIgnoresEveryRuntimeMode(t *testing.T) {
	base := provider.SessionOptions{
		Provider:        string(provider.ClaudeTUI),
		Model:           "claude-sonnet-5",
		WorkDir:         "/tmp/work",
		ReasoningEffort: provider.EffortHigh,
	}
	base.RuntimeMode = provider.AllRuntimeModes[0]
	want := ConfigFromOptions(base)

	for _, mode := range provider.AllRuntimeModes[1:] {
		opts := base
		opts.RuntimeMode = mode
		if got := ConfigFromOptions(opts); !reflect.DeepEqual(got, want) {
			t.Errorf("ConfigFromOptions with runtime mode %q = %+v, want %+v (runtime mode must be inert on claude-tui)", mode, got, want)
		}
	}
}
