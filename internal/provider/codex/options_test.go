package codex

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestCodexEffortFromOption(t *testing.T) {
	cases := []struct {
		effort provider.ReasoningEffort
		want   string
	}{
		{provider.EffortNone, "none"},
		{provider.EffortMinimal, "minimal"},
		{provider.EffortLow, "low"},
		{provider.EffortMedium, "medium"},
		{provider.EffortHigh, "high"},
		{provider.EffortXHigh, "xhigh"},
		{provider.EffortMax, ""},
		{"", ""}, // unknown / unset
	}
	for _, tc := range cases {
		t.Run(string(tc.effort), func(t *testing.T) {
			if got := codexEffortFromOption(tc.effort); got != tc.want {
				t.Errorf("codexEffortFromOption(%q) = %q, want %q", tc.effort, got, tc.want)
			}
		})
	}
}

// TestRuntimeModeToCodex enumerates the three runtime tiers and asserts the
// (approval, sandbox) pair each produces. Split-then-compose means a future
// RuntimeMode addition without touching both helpers is caught here.
func TestRuntimeModeToCodex(t *testing.T) {
	cases := map[provider.RuntimeMode]codexRuntime{
		provider.RuntimeApprovalRequired: {ApprovalPolicy: "untrusted", Sandbox: "read-only"},
		provider.RuntimeAutoAcceptEdits:  {ApprovalPolicy: "on-request", Sandbox: "workspace-write"},
		provider.RuntimeFullAccess:       {ApprovalPolicy: "never", Sandbox: "danger-full-access"},
	}
	for mode, want := range cases {
		t.Run(string(mode), func(t *testing.T) {
			got := runtimeModeToCodex(mode)
			if got != want {
				t.Errorf("runtimeModeToCodex(%q) = %+v, want %+v", mode, got, want)
			}
		})
	}
}

// TestConfigFromOptionsSystemPromptLandsOnBaseInstructions — Codex carries
// the system prompt via baseInstructions on thread/start. This test
// validates ConfigFromOptions copies SystemPrompt into SystemPrompt on the
// Config (buildThreadParams then maps SystemPrompt to the baseInstructions
// field; the field-name alignment test lives in session_helpers_test.go).
func TestConfigFromOptionsSystemPromptLandsOnBaseInstructions(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:     "codex",
		SystemPrompt: "Follow the codex playbook.",
	})
	if cfg.SystemPrompt != "Follow the codex playbook." {
		t.Errorf("SystemPrompt = %q, want 'Follow the codex playbook.'", cfg.SystemPrompt)
	}
}

// TestConfigFromOptionsRuntimeModesPair confirms the three-tier mapping
// writes both ApprovalPolicy and Sandbox in lockstep. This is the
// integration-y check that glues ConfigFromOptions to the canonical
// RuntimeMode → codex helpers.
func TestConfigFromOptionsRuntimeModesPair(t *testing.T) {
	cases := []struct {
		mode           provider.RuntimeMode
		approvalPolicy string
		sandbox        string
	}{
		{provider.RuntimeFullAccess, "never", "danger-full-access"},
		{provider.RuntimeAutoAcceptEdits, "on-request", "workspace-write"},
		{provider.RuntimeApprovalRequired, "untrusted", "read-only"},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider:    "codex",
				RuntimeMode: tc.mode,
			})
			if cfg.ApprovalPolicy != tc.approvalPolicy {
				t.Errorf("ApprovalPolicy = %q, want %q", cfg.ApprovalPolicy, tc.approvalPolicy)
			}
			if cfg.Sandbox != tc.sandbox {
				t.Errorf("Sandbox = %q, want %q", cfg.Sandbox, tc.sandbox)
			}
		})
	}
}

func TestConfigFromOptionsFastModePreservesModelAndSetsServiceTier(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "codex",
		Model:    "gpt-5.5",
		FastMode: true,
	})
	if cfg.Model != "gpt-5.5" {
		t.Errorf("Model = %q, want gpt-5.5", cfg.Model)
	}
	if cfg.ServiceTier != "fast" {
		t.Errorf("ServiceTier = %q, want fast", cfg.ServiceTier)
	}
}

func TestBuildThreadParamsThreadsServiceTier(t *testing.T) {
	params := buildThreadParams(Config{ServiceTier: "fast"})
	if params["serviceTier"] != "fast" {
		t.Errorf("serviceTier = %v, want fast", params["serviceTier"])
	}
}

func TestConfigFromOptionsFastModeOffOmitsServiceTier(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "codex",
		Model:    "gpt-5.4-mini",
		FastMode: false,
	})
	if cfg.ServiceTier != "" {
		t.Errorf("ServiceTier = %q, want empty", cfg.ServiceTier)
	}
}

func TestConfigFromOptionsFastModeUnsupportedModelOmitsServiceTier(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "codex",
		Model:    "gpt-5.4-mini",
		FastMode: true,
	})
	if cfg.ServiceTier != "" {
		t.Errorf("ServiceTier = %q, want empty for model without fast mode", cfg.ServiceTier)
	}
}

func TestConfigFromOptionsContextWindowAndAutoCompact(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "codex",
		Model:                      "gpt-5.4",
		ContextWindow:              provider.CodexExtendedContextWindow,
		AutoCompactExtendedPercent: 80,
	})
	if cfg.ContextWindow != provider.CodexExtendedContextWindow {
		t.Errorf("ContextWindow = %d, want %d", cfg.ContextWindow, provider.CodexExtendedContextWindow)
	}
	wantLimit := provider.CodexExtendedContextWindow * 80 / 100
	if cfg.AutoCompactTokenLimit != wantLimit {
		t.Errorf("AutoCompactTokenLimit = %d, want %d", cfg.AutoCompactTokenLimit, wantLimit)
	}
}

func TestConfigFromOptionsFastModeKeepsSelectedModelContext(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "codex",
		Model:                      "gpt-5.4",
		FastMode:                   true,
		ContextWindow:              provider.CodexExtendedContextWindow,
		AutoCompactStandardPercent: 70,
		AutoCompactExtendedPercent: 80,
	})
	if cfg.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want gpt-5.4", cfg.Model)
	}
	if cfg.ContextWindow != provider.CodexExtendedContextWindow {
		t.Errorf("ContextWindow = %d, want selected model extended context", cfg.ContextWindow)
	}
	wantLimit := provider.CodexExtendedContextWindow * 80 / 100
	if cfg.AutoCompactTokenLimit != wantLimit {
		t.Errorf("AutoCompactTokenLimit = %d, want %d", cfg.AutoCompactTokenLimit, wantLimit)
	}
}

func TestConfigFromOptionsClampsUnsupportedExtendedContext(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "codex",
		Model:                      "gpt-5.5",
		ContextWindow:              provider.CodexExtendedContextWindow,
		AutoCompactStandardPercent: 70,
		AutoCompactExtendedPercent: 80,
	})
	if cfg.ContextWindow != provider.CodexStandardContextWindow {
		t.Errorf("ContextWindow = %d, want selected model standard context", cfg.ContextWindow)
	}
	wantLimit := provider.CodexStandardContextWindow * 70 / 100
	if cfg.AutoCompactTokenLimit != wantLimit {
		t.Errorf("AutoCompactTokenLimit = %d, want %d", cfg.AutoCompactTokenLimit, wantLimit)
	}
}

// TestConfigFromOptionsResumeFlow — the Codex resume target is the
// thread-id we stored previously; it must survive the translation.
func TestConfigFromOptionsResumeFlow(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "codex",
		Resume:   "codex-thread-abc",
	})
	if cfg.ResumeThreadID != "codex-thread-abc" {
		t.Errorf("ResumeThreadID = %q, want codex-thread-abc", cfg.ResumeThreadID)
	}
}

// TestConfigFromOptionsForkSessionIgnored — ForkSession is explicitly a
// no-op for Codex (fork is a separate thread/fork app-server call). Pin
// that down so a future contributor reading the helper can't assume it's
// wired.
func TestConfigFromOptionsForkSessionIgnored(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "codex",
		Resume:      "codex-thread-abc",
		ForkSession: true,
	})
	// Config has no ForkSession field; the test value is that the
	// ResumeThreadID is still honoured unchanged (not e.g. cleared as a
	// side-effect of an attempted fork).
	if cfg.ResumeThreadID != "codex-thread-abc" {
		t.Errorf("ResumeThreadID = %q, want codex-thread-abc (fork ignored)", cfg.ResumeThreadID)
	}
}

// TestConfigFromOptionsReasoningEffortLands — the Codex Config surfaces
// ReasoningEffort so buildThreadParams can attach it to config.model_reasoning_effort
// and Send can attach it to turn/start's `effort`.
func TestConfigFromOptionsReasoningEffortLands(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:        "codex",
		ReasoningEffort: provider.EffortHigh,
	})
	if cfg.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %q, want high", cfg.ReasoningEffort)
	}
}

// TestBuildThreadParamsThreadsReasoningEffort — integration check: the
// config map passed to thread/start carries model_reasoning_effort under
// the `config` override bag when ReasoningEffort is non-empty.
func TestBuildThreadParamsThreadsReasoningEffort(t *testing.T) {
	params := buildThreadParams(Config{ReasoningEffort: "xhigh"})
	cfg, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("config override bag missing: %+v", params)
	}
	if cfg["model_reasoning_effort"] != "xhigh" {
		t.Errorf("config.model_reasoning_effort = %v, want xhigh", cfg["model_reasoning_effort"])
	}
}

func TestBuildThreadParamsThreadsContextOverrides(t *testing.T) {
	params := buildThreadParams(Config{
		ContextWindow:         provider.CodexExtendedContextWindow,
		AutoCompactTokenLimit: 800000,
	})
	cfg, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("config override bag missing: %+v", params)
	}
	if cfg["model_context_window"] != provider.CodexExtendedContextWindow {
		t.Errorf("config.model_context_window = %v, want %d", cfg["model_context_window"], provider.CodexExtendedContextWindow)
	}
	if cfg["model_auto_compact_token_limit"] != 800000 {
		t.Errorf("config.model_auto_compact_token_limit = %v, want 800000", cfg["model_auto_compact_token_limit"])
	}
}

// TestBuildThreadParamsOmitsReasoningEffortWhenEmpty — empty effort must
// NOT leak a bogus override value into the thread/start handshake.
func TestBuildThreadParamsOmitsReasoningEffortWhenEmpty(t *testing.T) {
	params := buildThreadParams(Config{})
	if _, ok := params["config"]; ok {
		t.Errorf("empty effort should not emit a config override bag; got %+v", params)
	}
}

// TestBuildThreadParamsMergesMCPServersAndEffort — when both MCP wiring and
// effort are present, they land in the same `config` map. Regression guard
// to make sure we don't overwrite mcp_servers when effort arrives.
func TestBuildThreadParamsMergesMCPServersAndEffort(t *testing.T) {
	mcp := map[string]any{"my-server": map[string]any{"command": "echo"}}
	params := buildThreadParams(Config{
		MCPServers:      mcp,
		ReasoningEffort: "high",
	})
	cfg, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("config bag missing: %+v", params)
	}
	if cfg["mcp_servers"] == nil {
		t.Errorf("mcp_servers missing from merged config: %+v", cfg)
	}
	if cfg["model_reasoning_effort"] != "high" {
		t.Errorf("effort missing from merged config: %+v", cfg)
	}
}

// TestBuildThreadParamsBaseInstructions — SystemPrompt must land on the
// baseInstructions key (camelCase). Matches ThreadStartParams.json.
func TestBuildThreadParamsBaseInstructions(t *testing.T) {
	params := buildThreadParams(Config{SystemPrompt: "hello"})
	if params["baseInstructions"] != "hello" {
		t.Errorf("baseInstructions = %v, want hello", params["baseInstructions"])
	}
}
