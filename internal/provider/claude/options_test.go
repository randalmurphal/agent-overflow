package claude

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// TestEffortSystemPrefix pins down the five-tier mapping claude uses to
// translate the composer's reasoning-effort enum into Claude's trigger-word
// convention. Low omits the prefix entirely; Max emits "ultrathink".
func TestEffortSystemPrefix(t *testing.T) {
	cases := []struct {
		effort provider.ReasoningEffort
		want   string
	}{
		{provider.EffortLow, ""},
		{provider.EffortMedium, "think"},
		{provider.EffortHigh, "think hard"},
		{provider.EffortXHigh, "think harder"},
		{provider.EffortMax, "ultrathink"},
	}
	for _, tc := range cases {
		t.Run(string(tc.effort), func(t *testing.T) {
			if got := effortSystemPrefix(tc.effort); got != tc.want {
				t.Errorf("effortSystemPrefix(%q) = %q, want %q", tc.effort, got, tc.want)
			}
		})
	}
}

// TestComposeSystemPromptPrefixAndBody covers the three non-trivial join
// paths: both present (separator inserted), only prefix (verbatim), only
// body (verbatim). Keeps the final prompt free of stray blank lines when
// one half is missing.
func TestComposeSystemPromptPrefixAndBody(t *testing.T) {
	if got := composeSystemPrompt("think hard", "Be helpful"); got != "think hard\n\nBe helpful" {
		t.Errorf("both: got %q", got)
	}
	if got := composeSystemPrompt("", "Be helpful"); got != "Be helpful" {
		t.Errorf("prompt only: got %q", got)
	}
	if got := composeSystemPrompt("ultrathink", ""); got != "ultrathink" {
		t.Errorf("prefix only: got %q", got)
	}
	if got := composeSystemPrompt("", ""); got != "" {
		t.Errorf("empty: got %q", got)
	}
}

// TestEnvForContextWindow confirms the 1M-token toggle sets the opt-in beta
// header and that 200k (or zero / unknown) leaves env untouched so the caller
// doesn't accidentally ship the beta header.
func TestEnvForContextWindow(t *testing.T) {
	if env := envForContextWindow(1000000); env["ANTHROPIC_BETAS"] != claudeOneMillionContextBeta {
		t.Errorf("1M should set ANTHROPIC_BETAS=%q, got %v", claudeOneMillionContextBeta, env)
	}
	if env := envForContextWindow(200000); env != nil {
		t.Errorf("200k should not set env; got %v", env)
	}
	if env := envForContextWindow(0); env != nil {
		t.Errorf("zero should not set env; got %v", env)
	}
	if env := envForContextWindow(500000); env != nil {
		t.Errorf("unknown size %d should not set env; got %v", 500000, env)
	}
}

// TestFastModelForClaude covers the intentional swap to Haiku when Opus is
// selected (fast-mode on) and the pass-through when the user has already
// picked a cheap model.
func TestFastModelForClaude(t *testing.T) {
	cases := []struct {
		current string
		want    string
	}{
		{"claude-opus-4-6", "claude-haiku-4-5"},
		{"claude-opus-4-7", "claude-haiku-4-5"},
		{"CLAUDE-OPUS-5", "claude-haiku-4-5"}, // case insensitive
		{"", "claude-haiku-4-5"},              // unset → assume Anthropic default is opus-class
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"claude-haiku-4-5", "claude-haiku-4-5"},
	}
	for _, tc := range cases {
		t.Run(tc.current, func(t *testing.T) {
			if got := fastModelForClaude(tc.current); got != tc.want {
				t.Errorf("fastModelForClaude(%q) = %q, want %q", tc.current, got, tc.want)
			}
		})
	}
}

// TestConfigFromOptionsEffortPrefixPrepended validates that the effort
// prefix lands BEFORE the caller-composed system prompt, with the expected
// blank-line separator. This is the property the prompt-prefix contract
// depends on.
func TestConfigFromOptionsEffortPrefixPrepended(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:        "claude",
		ReasoningEffort: provider.EffortMax,
		SystemPrompt:    "You are the agent.",
	})
	want := "ultrathink\n\nYou are the agent."
	if cfg.SystemPrompt != want {
		t.Errorf("SystemPrompt = %q, want %q", cfg.SystemPrompt, want)
	}
}

// TestConfigFromOptionsEffortLowNoPrefix confirms that the low tier doesn't
// sneak a "think" token in front of the system prompt — low is the quietest
// tier and MUST NOT nudge the model.
func TestConfigFromOptionsEffortLowNoPrefix(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:        "claude",
		ReasoningEffort: provider.EffortLow,
		SystemPrompt:    "You are the agent.",
	})
	if cfg.SystemPrompt != "You are the agent." {
		t.Errorf("SystemPrompt = %q, want unchanged for low tier", cfg.SystemPrompt)
	}
}

// TestConfigFromOptionsOneMillionContextSetsEnv is the regression guard
// around the ANTHROPIC_BETAS header opt-in.
func TestConfigFromOptionsOneMillionContextSetsEnv(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:      "claude",
		ContextWindow: 1000000,
	})
	if cfg.Env["ANTHROPIC_BETAS"] != claudeOneMillionContextBeta {
		t.Errorf("Env[ANTHROPIC_BETAS] = %q, want %q", cfg.Env["ANTHROPIC_BETAS"], claudeOneMillionContextBeta)
	}
}

// TestConfigFromOptionsTwoHundredKContextOmitsEnv — 200k is the default; we
// MUST NOT ship the beta header in that case.
func TestConfigFromOptionsTwoHundredKContextOmitsEnv(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:      "claude",
		ContextWindow: 200000,
	})
	if cfg.Env != nil {
		t.Errorf("Env = %v, want nil for 200k", cfg.Env)
	}
}

// TestConfigFromOptionsRuntimeModeFullAccessFlag pins down the
// full-access → --dangerously-skip-permissions mapping at the
// ConfigFromOptions boundary.
func TestConfigFromOptionsRuntimeModeFullAccessFlag(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeFullAccess,
	})
	if len(cfg.PermissionFlags) != 1 || cfg.PermissionFlags[0] != "--dangerously-skip-permissions" {
		t.Errorf("PermissionFlags = %v, want [--dangerously-skip-permissions]", cfg.PermissionFlags)
	}
}

// TestConfigFromOptionsRuntimeModeApprovalRequiredNoFlag confirms the safest
// tier emits no permission flag so the CLI's own default prompting kicks in.
func TestConfigFromOptionsRuntimeModeApprovalRequiredNoFlag(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeApprovalRequired,
	})
	if cfg.PermissionFlags != nil {
		t.Errorf("PermissionFlags = %v, want nil for approval-required", cfg.PermissionFlags)
	}
}

// TestConfigFromOptionsRuntimeModeAutoAcceptEdits sanity-checks the middle
// tier sends `--permission-mode acceptEdits`.
func TestConfigFromOptionsRuntimeModeAutoAcceptEdits(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeAutoAcceptEdits,
	})
	want := []string{"--permission-mode", "acceptEdits"}
	if len(cfg.PermissionFlags) != len(want) ||
		cfg.PermissionFlags[0] != want[0] || cfg.PermissionFlags[1] != want[1] {
		t.Errorf("PermissionFlags = %v, want %v", cfg.PermissionFlags, want)
	}
}

// TestConfigFromOptionsFastModeSwapsOpusToHaiku exercises the auto-swap path;
// the caller may have shipped opus, fast-mode flips it to haiku so the thread
// doesn't quietly ignore the toggle.
func TestConfigFromOptionsFastModeSwapsOpusToHaiku(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "claude",
		Model:    "claude-opus-4-6",
		FastMode: true,
	})
	if cfg.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5", cfg.Model)
	}
}

// TestConfigFromOptionsFastModeLeavesHaikuAndSonnetUntouched is the inverse —
// the user already picked a cheap model, fast-mode shouldn't rewrite it.
func TestConfigFromOptionsFastModeLeavesHaikuAndSonnetUntouched(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-6", "claude-haiku-4-5"} {
		cfg := ConfigFromOptions(provider.SessionOptions{
			Provider: "claude",
			Model:    model,
			FastMode: true,
		})
		if cfg.Model != model {
			t.Errorf("fast mode rewrote %q to %q", model, cfg.Model)
		}
	}
}

// TestConfigFromOptionsFastModeOffPreservesModel — the negative control: if
// fast mode is off, we pass the model through verbatim even if it's opus.
func TestConfigFromOptionsFastModeOffPreservesModel(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "claude",
		Model:    "claude-opus-4-6",
		FastMode: false,
	})
	if cfg.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want claude-opus-4-6 (fast mode off)", cfg.Model)
	}
}

// TestConfigFromOptionsResumeAndForkFlow — the resume target and fork flag
// have to survive the translation so the session-start plumbing keeps
// behaving after restart and pending-fork consumption.
func TestConfigFromOptionsResumeAndForkFlow(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		Resume:      "session-uuid",
		ForkSession: true,
	})
	if cfg.Resume != "session-uuid" {
		t.Errorf("Resume = %q, want session-uuid", cfg.Resume)
	}
	if !cfg.ForkSession {
		t.Error("ForkSession = false, want true")
	}
}

// TestConfigFromOptionsThreadsIntoBuildArgs is a lightweight integration
// check that the options → config → args chain actually produces the flags
// we expect a real `claude` invocation to include. Prevents a regression
// where PermissionFlags stops flowing into buildArgs.
func TestConfigFromOptionsThreadsIntoBuildArgs(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:        "claude",
		Model:           "claude-opus-4-6",
		ReasoningEffort: provider.EffortHigh,
		SystemPrompt:    "Be an agent.",
		RuntimeMode:     provider.RuntimeFullAccess,
		ContextWindow:   1000000,
	})
	args := buildArgs(cfg)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Errorf("args missing --dangerously-skip-permissions: %v", args)
	}
	if !strings.Contains(joined, "--system-prompt think hard\n\nBe an agent.") {
		// The prompt contains a literal newline, so exact substring matching
		// is brittle — we just spot-check that the prefix landed in the
		// system-prompt value by scanning the flag → value pair directly.
		foundPrefix := false
		for i, a := range args {
			if a == "--system-prompt" && i+1 < len(args) &&
				strings.HasPrefix(args[i+1], "think hard") {
				foundPrefix = true
				break
			}
		}
		if !foundPrefix {
			t.Errorf("system prompt did not carry 'think hard' prefix: %v", args)
		}
	}

	if cfg.Env["ANTHROPIC_BETAS"] != claudeOneMillionContextBeta {
		t.Errorf("Env[ANTHROPIC_BETAS] missing after ConfigFromOptions: %v", cfg.Env)
	}
}
