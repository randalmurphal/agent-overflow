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

// The two settings-owned axes (docs/specs/prompt-tool-overrides.md) reach the
// TUI launch Config: the interactive CLI honors `--system-prompt-file` and
// `--disallowedTools` exactly as headless does (spike-verified 2.1.234), so
// settings.PromptOverridesForProvider / DisabledToolsForProvider route
// claude-tui onto the Claude lists and this is where they land.
//
// The list is taken RAW rather than off claude.ConfigFromOptions's merged
// field, but it still goes through the shared argv-safety pass: a name that
// is not ONE safe CLI argument must be impossible to reach argv on either
// transport.
func TestConfigFromOptionsCarriesTheSettingsOwnedAxes(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:     string(provider.ClaudeTUI),
		Model:        "claude-opus-5",
		WorkDir:      "/tmp/work",
		SystemPrompt: "You are the agent.",
		DisabledTools: []string{
			"Workflow",
			"  WebSearch  ", // trimmed
			"",              // dropped
			"   ",           // dropped
			"two words",     // dropped: not one CLI argument
			"-rf",           // dropped: parses as a flag
			"Workflow",      // deduped
		},
	})
	if cfg.SystemPrompt != "You are the agent." {
		t.Fatalf("SystemPrompt = %q, want the override to pass through", cfg.SystemPrompt)
	}
	want := []string{"Workflow", "WebSearch"}
	if !reflect.DeepEqual(cfg.DisallowedTools, want) {
		t.Fatalf("DisallowedTools = %v, want %v", cfg.DisallowedTools, want)
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
		// A non-empty settings list is what makes the read-only tier's
		// union path reachable at all on the headless side, so seeding it
		// here is what turns "the mode strip leaked in" into a failure
		// rather than a coincidence of an empty list.
		DisabledTools: []string{"Workflow"},
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

// The peer-inbox axes reach the TUI Config through claude.ConfigFromOptions,
// so the two Claude transports resolve the policy identically — including the
// stated refusal when the feature is OFF, which is the whole point of that
// resolution (a remote GrowthBook flag can bind the inbox for a user who
// never enabled it here, and only an explicit key says otherwise).
func TestConfigFromOptionsCarriesTheCrossSessionAxes(t *testing.T) {
	on := ConfigFromOptions(provider.SessionOptions{
		Provider:           string(provider.ClaudeTUI),
		Model:              "claude-opus-5",
		ClaudeCrossSession: provider.ClaudeCrossSession{Enabled: true, Inbound: "refuse"},
	})
	if !on.CrossSessionEnabled {
		t.Fatal("CrossSessionEnabled = false for an enabled setting")
	}
	if on.CrossSessionInbound != "refuse" {
		t.Fatalf("CrossSessionInbound = %q, want refuse", on.CrossSessionInbound)
	}

	off := ConfigFromOptions(provider.SessionOptions{
		Provider: string(provider.ClaudeTUI),
		Model:    "claude-opus-5",
	})
	if off.CrossSessionEnabled {
		t.Fatal("CrossSessionEnabled = true with the setting off")
	}
	if off.CrossSessionInbound != "refuse" {
		t.Fatalf("CrossSessionInbound = %q with the setting off, want the stated refusal", off.CrossSessionInbound)
	}
}
