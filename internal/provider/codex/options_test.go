package codex

import (
	"bytes"
	"log"
	"testing"

	"agent-overflow/internal/provider"
)

// TestCodexEffortFromOption enumerates every tier of ReasoningEffort and
// pins down Codex's response: Low→low, Medium→medium, High→high, XHigh→xhigh,
// Max→xhigh (floored, with a log entry). Empty/unknown returns "".
func TestCodexEffortFromOption(t *testing.T) {
	cases := []struct {
		effort provider.ReasoningEffort
		want   string
	}{
		{provider.EffortLow, "low"},
		{provider.EffortMedium, "medium"},
		{provider.EffortHigh, "high"},
		{provider.EffortXHigh, "xhigh"},
		{provider.EffortMax, "xhigh"}, // floored
		{"", ""},                      // unknown / unset
	}
	for _, tc := range cases {
		t.Run(string(tc.effort), func(t *testing.T) {
			if got := codexEffortFromOption(tc.effort); got != tc.want {
				t.Errorf("codexEffortFromOption(%q) = %q, want %q", tc.effort, got, tc.want)
			}
		})
	}
}

// TestCodexEffortMaxLogsFloor confirms the floor is observable — we log once
// so the user's "why did my max thread only use xhigh" question has an
// answer without needing a full-session trace.
func TestCodexEffortMaxLogsFloor(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	_ = codexEffortFromOption(provider.EffortMax)

	if !bytes.Contains(buf.Bytes(), []byte("mapped to xhigh")) {
		t.Errorf("expected floor log, got %q", buf.String())
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

// TestFastModelForCodex exercises the swap and the pass-through paths. The
// swap is gpt-5*.whatever → gpt-5.4-mini when the id isn't already a mini;
// o3 / o4-mini stay as-is (different family; Fast Mode doesn't imply a
// family switch).
func TestFastModelForCodex(t *testing.T) {
	cases := []struct {
		current string
		want    string
	}{
		{"gpt-5.5", "gpt-5.4-mini"},
		{"gpt-5.4", "gpt-5.4-mini"},
		{"gpt-5", "gpt-5.4-mini"},
		{"gpt-5-turbo", "gpt-5.4-mini"},
		{"gpt-5.4-mini", "gpt-5.4-mini"}, // already cheap
		{"o4-mini", "o4-mini"},           // mini but wrong family → leave alone
		{"o3", "o3"},                     // different family
		{"", ""},                         // unset → let Codex pick
	}
	for _, tc := range cases {
		t.Run(tc.current, func(t *testing.T) {
			if got := fastModelForCodex(tc.current); got != tc.want {
				t.Errorf("fastModelForCodex(%q) = %q, want %q", tc.current, got, tc.want)
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

// TestConfigFromOptionsFastModeSwaps exercises the gpt-5.5 → gpt-5.4-mini
// swap at the translation boundary.
func TestConfigFromOptionsFastModeSwaps(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "codex",
		Model:    "gpt-5.5",
		FastMode: true,
	})
	if cfg.Model != "gpt-5.4-mini" {
		t.Errorf("Model = %q, want gpt-5.4-mini", cfg.Model)
	}
}

// TestConfigFromOptionsFastModeMiniPassesThrough — already-mini stays put.
func TestConfigFromOptionsFastModeMiniPassesThrough(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "codex",
		Model:    "gpt-5.4-mini",
		FastMode: true,
	})
	if cfg.Model != "gpt-5.4-mini" {
		t.Errorf("Model = %q, want unchanged mini", cfg.Model)
	}
}

func TestConfigFromOptionsContextWindowAndAutoCompact(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "codex",
		Model:                      "gpt-5.5",
		ContextWindow:              1050000,
		AutoCompactExtendedPercent: 80,
	})
	if cfg.ContextWindow != 1050000 {
		t.Errorf("ContextWindow = %d, want 1050000", cfg.ContextWindow)
	}
	if cfg.AutoCompactTokenLimit != 840000 {
		t.Errorf("AutoCompactTokenLimit = %d, want 840000", cfg.AutoCompactTokenLimit)
	}
}

func TestConfigFromOptionsFastModeFallsBackToEffectiveModelContext(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "codex",
		Model:                      "gpt-5.5",
		FastMode:                   true,
		ContextWindow:              1050000,
		AutoCompactStandardPercent: 70,
		AutoCompactExtendedPercent: 80,
	})
	if cfg.Model != "gpt-5.4-mini" {
		t.Fatalf("Model = %q, want gpt-5.4-mini", cfg.Model)
	}
	if cfg.ContextWindow != 272000 {
		t.Errorf("ContextWindow = %d, want gpt-5.4-mini standard 272000", cfg.ContextWindow)
	}
	if cfg.AutoCompactTokenLimit != 190400 {
		t.Errorf("AutoCompactTokenLimit = %d, want 190400", cfg.AutoCompactTokenLimit)
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
		ContextWindow:         1050000,
		AutoCompactTokenLimit: 840000,
	})
	cfg, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("config override bag missing: %+v", params)
	}
	if cfg["model_context_window"] != 1050000 {
		t.Errorf("config.model_context_window = %v, want 1050000", cfg["model_context_window"])
	}
	if cfg["model_auto_compact_token_limit"] != 840000 {
		t.Errorf("config.model_auto_compact_token_limit = %v, want 840000", cfg["model_auto_compact_token_limit"])
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
