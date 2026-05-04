package claude

import (
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestClaudeEffortFromOption(t *testing.T) {
	cases := []struct {
		effort provider.ReasoningEffort
		want   string
	}{
		{provider.EffortLow, "low"},
		{provider.EffortMedium, "medium"},
		{provider.EffortHigh, "high"},
		{provider.EffortXHigh, "max"},
		{provider.EffortMax, "max"},
		{provider.EffortNone, ""},
		{provider.EffortMinimal, ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.effort), func(t *testing.T) {
			if got := claudeEffortFromOption(tc.effort); got != tc.want {
				t.Errorf("claudeEffortFromOption(%q) = %q, want %q", tc.effort, got, tc.want)
			}
		})
	}
}

func TestClaudeModelForContextWindow(t *testing.T) {
	if got := claudeModelForContextWindow("claude-opus-4-7", provider.ClaudeExtendedContextWindow); got != "claude-opus-4-7[1m]" {
		t.Fatalf("extended model = %q, want [1m] suffix", got)
	}
	if got := claudeModelForContextWindow("claude-opus-4-7", provider.ClaudeStandardContextWindow); got != "claude-opus-4-7" {
		t.Fatalf("standard model = %q, want unchanged", got)
	}
}

func TestConfigFromOptionsFastModePreservesModelAndSetsFlag(t *testing.T) {
	for _, model := range []string{"claude-opus-4-7", "claude-opus-4-6"} {
		t.Run(model, func(t *testing.T) {
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider: "claude",
				Model:    model,
				FastMode: true,
			})
			if cfg.Model != model {
				t.Fatalf("Model = %q, want %s", cfg.Model, model)
			}
			if !cfg.FastMode {
				t.Fatal("FastMode = false, want true")
			}
		})
	}
}

func TestConfigFromOptionsFastModeUnsupportedModelClearsFlag(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "claude",
		Model:    "claude-opus-4-5",
		FastMode: true,
	})
	if cfg.FastMode {
		t.Fatal("FastMode = true, want false for opus 4.5")
	}
}

func TestConfigFromOptionsEffortUsesNativeFlag(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:        "claude",
		Model:           "claude-opus-4-6",
		ReasoningEffort: provider.EffortHigh,
		SystemPrompt:    "You are the agent.",
	})
	if cfg.SystemPrompt != "You are the agent." {
		t.Fatalf("SystemPrompt = %q, want unchanged", cfg.SystemPrompt)
	}
	if cfg.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", cfg.ReasoningEffort)
	}
}

func TestConfigFromOptionsOneMillionContextUsesModelSuffix(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:      "claude",
		Model:         "claude-opus-4-7",
		ContextWindow: provider.ClaudeExtendedContextWindow,
	})
	if cfg.Model != "claude-opus-4-7[1m]" {
		t.Fatalf("Model = %q, want claude-opus-4-7[1m]", cfg.Model)
	}
	if cfg.Env != nil {
		t.Fatalf("Env = %v, want nil for 1m suffix path", cfg.Env)
	}
}

func TestConfigFromOptionsAutoCompactEnv(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "claude",
		Model:                      "claude-opus-4-7",
		ContextWindow:              provider.ClaudeExtendedContextWindow,
		AutoCompactExtendedPercent: 80,
	})
	if cfg.Env["CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"] != "80" {
		t.Fatalf("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE = %q, want 80", cfg.Env["CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"])
	}
}

func TestConfigFromOptionsRuntimeModeFullAccessFlag(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeFullAccess,
	})
	want := []string{"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions"}
	if !slices.Equal(cfg.PermissionFlags, want) {
		t.Errorf("PermissionFlags = %v, want %v", cfg.PermissionFlags, want)
	}
	if cfg.BasePermissionMode != "bypassPermissions" {
		t.Errorf("BasePermissionMode = %q, want bypassPermissions", cfg.BasePermissionMode)
	}
}

func TestConfigFromOptionsRuntimeModeApprovalRequiredNoFlag(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeApprovalRequired,
	})
	if cfg.PermissionFlags != nil {
		t.Errorf("PermissionFlags = %v, want nil for approval-required", cfg.PermissionFlags)
	}
}

func TestConfigFromOptionsRuntimeModeAutoAcceptEdits(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeAutoAcceptEdits,
	})
	want := []string{"--permission-mode", "acceptEdits"}
	if !slices.Equal(cfg.PermissionFlags, want) {
		t.Errorf("PermissionFlags = %v, want %v", cfg.PermissionFlags, want)
	}
}

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

func TestConfigFromOptionsThreadsIntoBuildArgs(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:        "claude",
		Model:           "claude-opus-4-6",
		ReasoningEffort: provider.EffortHigh,
		SystemPrompt:    "Be an agent.",
		RuntimeMode:     provider.RuntimeFullAccess,
		ContextWindow:   provider.ClaudeExtendedContextWindow,
		FastMode:        true,
	})
	args := buildArgs(cfg)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model claude-opus-4-6[1m]") {
		t.Fatalf("args missing suffixed model: %v", args)
	}
	if !strings.Contains(joined, "--effort high") {
		t.Fatalf("args missing effort: %v", args)
	}
	if !strings.Contains(joined, `--settings {"fastMode":true}`) {
		t.Fatalf("args missing fast mode settings: %v", args)
	}
	if !strings.Contains(joined, "--permission-mode bypassPermissions --allow-dangerously-skip-permissions") {
		t.Fatalf("args missing bypass permission flags: %v", args)
	}
}
