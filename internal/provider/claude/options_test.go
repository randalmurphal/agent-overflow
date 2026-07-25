package claude

import (
	"encoding/json"
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
		{provider.EffortXHigh, "xhigh"},
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

func TestConfigFromOptionsCopiesResumeAt(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "claude",
		Resume:   "session-123",
		ResumeAt: "leaf-456",
	})
	if cfg.Resume != "session-123" {
		t.Fatalf("Resume = %q, want session-123", cfg.Resume)
	}
	if cfg.ResumeAt != "leaf-456" {
		t.Fatalf("ResumeAt = %q, want leaf-456", cfg.ResumeAt)
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
	if cfg.AutoCompactPercent != 0 {
		t.Fatalf("AutoCompactPercent = %d, want 0 for unset auto-compact", cfg.AutoCompactPercent)
	}
	if cfg.ContextWindow != provider.ClaudeExtendedContextWindow {
		t.Fatalf("ContextWindow = %d, want %d (resolved window carried on Config)", cfg.ContextWindow, provider.ClaudeExtendedContextWindow)
	}
}

func TestConfigFromOptionsAutoCompactPercentExtendedTier(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "claude",
		Model:                      "claude-opus-4-7",
		ContextWindow:              provider.ClaudeExtendedContextWindow,
		AutoCompactExtendedPercent: 80,
	})
	if cfg.AutoCompactPercent != 80 {
		t.Fatalf("AutoCompactPercent = %d, want 80 (extended tier)", cfg.AutoCompactPercent)
	}
}

func TestConfigFromOptionsAutoCompactPercentStandardTier(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "claude",
		Model:                      "claude-opus-4-7",
		AutoCompactStandardPercent: 50,
	})
	if cfg.AutoCompactPercent != 50 {
		t.Fatalf("AutoCompactPercent = %d, want 50 (standard tier)", cfg.AutoCompactPercent)
	}
}

func TestBuildArgsAutoCompactRendersThroughSettingsFlag(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "claude",
		Model:                      "claude-opus-4-7",
		AutoCompactStandardPercent: 50,
	})
	args := buildArgs(cfg)
	joined := strings.Join(args, " ")
	want := `--settings {"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"50","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"200000"}}`
	if !strings.Contains(joined, want) {
		t.Fatalf("args missing autocompact-via-flag-settings: got=%v\nwant substring=%q", args, want)
	}
}

// TestBuildArgsAutoCompactSendsWindowForExtendedTier pins the companion
// CLAUDE_CODE_AUTO_COMPACT_WINDOW value to the thread's RESOLVED window:
// claude ≥2.1.201 refuses to auto-compact at all unless an explicit
// auto-compact window resolves, so the pct override alone is a no-op.
// The extended tier must send 1000000, not the 200k default.
func TestBuildArgsAutoCompactSendsWindowForExtendedTier(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "claude",
		Model:                      "claude-opus-4-7",
		ContextWindow:              provider.ClaudeExtendedContextWindow,
		AutoCompactExtendedPercent: 40,
	})
	args := buildArgs(cfg)
	joined := strings.Join(args, " ")
	want := `--settings {"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"40","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"1000000"}}`
	if !strings.Contains(joined, want) {
		t.Fatalf("args missing extended-tier autocompact settings: got=%v\nwant substring=%q", args, want)
	}
}

// TestBuildArgsAutoCompactOmitsWindowWhenUnresolved covers hand-built
// Configs that bypass ConfigFromOptions: a percent with no known window
// still renders the pct override but must not invent a window value.
func TestBuildArgsAutoCompactOmitsWindowWhenUnresolved(t *testing.T) {
	cfg := Config{AutoCompactPercent: 50}
	args := buildArgs(cfg)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, `"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"50"`) {
		t.Fatalf("args missing pct override: %v", args)
	}
	if strings.Contains(joined, "CLAUDE_CODE_AUTO_COMPACT_WINDOW") {
		t.Fatalf("zero ContextWindow must omit the window env var: %v", args)
	}
}

func TestBuildArgsCombinesFastModeAndAutoCompactInOneSettingsFlag(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                   "claude",
		Model:                      "claude-opus-4-7",
		FastMode:                   true,
		AutoCompactStandardPercent: 50,
	})
	args := buildArgs(cfg)
	joined := strings.Join(args, " ")
	want := `--settings {"fastMode":true,"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"50","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"200000"}}`
	if !strings.Contains(joined, want) {
		t.Fatalf("args missing combined fastMode + autocompact settings: got=%v\nwant substring=%q", args, want)
	}
	if got := strings.Count(joined, "--settings"); got != 1 {
		t.Fatalf("expected exactly one --settings flag, got %d in %v", got, args)
	}
}

func TestBuildArgsOmitsSettingsFlagWhenNothingSet(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "claude",
		Model:    "claude-opus-4-7",
	})
	args := buildArgs(cfg)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--settings") {
		t.Fatalf("expected no --settings flag when fastMode/autocompact unset, got %v", args)
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
	// Pin the omitempty contract: with no AutoCompactPercent set, the
	// rendered settings must not carry an "env" block. Without this
	// assertion the substring above would still match a payload like
	// `--settings {"fastMode":true,"env":{...}}`, masking accidental
	// env-block leakage from unrelated state.
	if strings.Contains(joined, `"env":`) {
		t.Fatalf("fastMode-only path leaked env block into --settings: %v", args)
	}
	if !strings.Contains(joined, "--permission-mode bypassPermissions --allow-dangerously-skip-permissions") {
		t.Fatalf("args missing bypass permission flags: %v", args)
	}
}

// TestMcpConfigForCLIBackfillsHTTPType pins the contract that lets the
// design package share one MCPServer struct between Codex (untagged
// {"url":...} via inline mcp_servers) and Claude (--mcp-config requires
// `"type": "http"`). The shared shape is the canonical untagged one;
// this helper is what makes Claude's CLI accept it.
func TestMcpConfigForCLIBackfillsHTTPType(t *testing.T) {
	cfg := Config{
		MCPServers: map[string]any{
			"design": map[string]any{
				"url": "http://127.0.0.1:1234/mcp/thread-A",
			},
		},
	}
	out, ok := mcpConfigForCLI(cfg)
	if !ok {
		t.Fatal("mcpConfigForCLI returned ok=false for non-empty servers")
	}
	var payload struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("unmarshal: %v\nout: %s", err, out)
	}
	server := payload.MCPServers["design"]
	if server == nil {
		t.Fatalf("design server missing from %s", out)
	}
	if got := server["type"]; got != "http" {
		t.Fatalf("server type = %v, want \"http\" — Claude CLI rejects http servers without it (%s)", got, out)
	}
	if got := server["url"]; got != "http://127.0.0.1:1234/mcp/thread-A" {
		t.Fatalf("server url = %v, want unchanged passthrough", got)
	}
}

func TestBuildArgsCanMergeFirstPartyMCPWithNativeDiscovery(t *testing.T) {
	cfg := Config{
		MCPServers: map[string]any{
			"workflows": HTTPMCPServer("http://127.0.0.1/mcp", map[string]string{"Authorization": "Bearer token"}),
		},
		MergeMCPServers: true,
	}
	args := buildArgs(cfg)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--mcp-config") {
		t.Fatalf("args missing --mcp-config: %v", args)
	}
	if strings.Contains(joined, "--strict-mcp-config") {
		t.Fatalf("merge args unexpectedly disable native discovery: %v", args)
	}
	if !strings.Contains(joined, `"headers":{"Authorization":"Bearer token"}`) {
		t.Fatalf("args missing HTTP auth headers: %v", args)
	}
}

func TestMcpConfigForCLIPreservesExplicitType(t *testing.T) {
	cfg := Config{
		MCPServers: map[string]any{
			"sse-server": map[string]any{
				"type": "sse",
				"url":  "http://example.com/sse",
			},
		},
	}
	out, _ := mcpConfigForCLI(cfg)
	var payload struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := payload.MCPServers["sse-server"]["type"]; got != "sse" {
		t.Fatalf("explicit type clobbered: got %v, want \"sse\"", got)
	}
}

func TestMcpConfigForCLIBackfillsStdioType(t *testing.T) {
	cfg := Config{
		MCPServers: map[string]any{
			"local": map[string]any{
				"command": "/usr/local/bin/my-mcp",
				"args":    []any{"--flag"},
			},
		},
	}
	out, _ := mcpConfigForCLI(cfg)
	var payload struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := payload.MCPServers["local"]["type"]; got != "stdio" {
		t.Fatalf("stdio backfill missing: got %v, want \"stdio\"", got)
	}
}

func TestBuildArgsAutoCompactClampsAbove90(t *testing.T) {
	// Direct Config construction bypasses ConfigFromOptions's upstream
	// normalizer so we exercise inlineSettingsForCLI's defensive clamp
	// directly. A future regression that drops the clamp (or changes
	// the upper bound) would show up here.
	cfg := Config{AutoCompactPercent: 150}
	args := buildArgs(cfg)
	joined := strings.Join(args, " ")
	want := `"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"90"`
	if !strings.Contains(joined, want) {
		t.Fatalf("AutoCompactPercent=150 should render as %q, got %v", want, args)
	}
	if strings.Contains(joined, `"150"`) {
		t.Fatalf("unclamped 150 leaked into --settings: %v", args)
	}
}

// TestConfigFromOptionsRuntimeModeReadOnly pins the verified read-only
// launch config. Both halves are load-bearing and neither is sufficient
// alone:
//
//   - `--permission-mode dontAsk` turns every would-be prompt into an
//     immediate denial, so an unattended turn is refused rather than left
//     hanging on a CanUseTool control_request nobody answers.
//   - `--disallowedTools Write,Edit,NotebookEdit` removes the write tools
//     from the session outright. dontAsk only converts "ask" to "deny", so
//     an action a settings source already ALLOWS never becomes an ask and
//     would be permitted — a user's `permissions.allow: ["Write"]` would
//     otherwise let a read-only session write files.
//
// Verified against claude 2.1.219; transcript in
// docs/references/claude-wire.md §"Permission modes for read-only sessions".
func TestConfigFromOptionsRuntimeModeReadOnly(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeReadOnly,
	})
	wantFlags := []string{"--permission-mode", "dontAsk"}
	if !slices.Equal(cfg.PermissionFlags, wantFlags) {
		t.Errorf("PermissionFlags = %v, want %v", cfg.PermissionFlags, wantFlags)
	}
	wantTools := []string{"Write", "Edit", "NotebookEdit"}
	if !slices.Equal(cfg.DisallowedTools, wantTools) {
		t.Errorf("DisallowedTools = %v, want %v", cfg.DisallowedTools, wantTools)
	}
	if cfg.BasePermissionMode != "dontAsk" {
		t.Errorf("BasePermissionMode = %q, want dontAsk", cfg.BasePermissionMode)
	}
	// The bypass escape hatch must never ride along with a restricted mode.
	if slices.Contains(cfg.PermissionFlags, "--allow-dangerously-skip-permissions") {
		t.Error("read-only must not carry --allow-dangerously-skip-permissions")
	}
}

// TestOnlyReadOnlyDisallowsTools proves the tool removal is exclusive to the
// read-only tier. Leaking it into another mode would silently strip Write /
// Edit from an interactive session.
func TestOnlyReadOnlyDisallowsTools(t *testing.T) {
	for _, mode := range provider.AllRuntimeModes {
		cfg := ConfigFromOptions(provider.SessionOptions{Provider: "claude", RuntimeMode: mode})
		if mode == provider.RuntimeReadOnly {
			if len(cfg.DisallowedTools) == 0 {
				t.Errorf("mode %q: expected disallowed tools", mode)
			}
			continue
		}
		if len(cfg.DisallowedTools) != 0 {
			t.Errorf("mode %q: DisallowedTools = %v, want none", mode, cfg.DisallowedTools)
		}
	}
}

// TestClaudeBasePermissionModeCoversEveryRuntimeMode makes the mapping
// exhaustive. A mode that falls through to "default" would spawn a
// supervised, prompting session — for an unattended workflow phase that is a
// hang, not a refusal.
func TestClaudeBasePermissionModeCoversEveryRuntimeMode(t *testing.T) {
	want := map[provider.RuntimeMode]string{
		provider.RuntimeReadOnly:         "dontAsk",
		provider.RuntimeApprovalRequired: "default",
		provider.RuntimeAutoAcceptEdits:  "acceptEdits",
		provider.RuntimeFullAccess:       "bypassPermissions",
	}
	for _, mode := range provider.AllRuntimeModes {
		expected, ok := want[mode]
		if !ok {
			t.Fatalf("runtime mode %q has no asserted permission mode — add one here and in claudeBasePermissionMode", mode)
		}
		if got := claudeBasePermissionMode(mode); got != expected {
			t.Errorf("claudeBasePermissionMode(%q) = %q, want %q", mode, got, expected)
		}
	}
}

// TestBuildArgsRendersDisallowedTools proves the Config field actually
// reaches argv. A mapping the spawn path drops is enforcement on paper only.
func TestBuildArgsRendersDisallowedTools(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeReadOnly,
	})
	args := buildArgs(cfg)
	for _, tool := range []string{"Write", "Edit", "NotebookEdit"} {
		idx := slices.Index(args, tool)
		if idx <= 0 || args[idx-1] != "--disallowedTools" {
			t.Errorf("args missing --disallowedTools %s: %v", tool, args)
		}
	}
	modeIdx := slices.Index(args, "dontAsk")
	if modeIdx <= 0 || args[modeIdx-1] != "--permission-mode" {
		t.Errorf("args missing --permission-mode dontAsk: %v", args)
	}
}

// TestNormalizeClaudePermissionModeAcceptsEverySelectableMode is the guard
// for the coercion at the session boundary: any mode ConfigFromOptions can
// emit must survive normalizeClaudePermissionMode unchanged. A mode that
// collapses to "default" there would make the live session's restored base
// mode (after a plan turn) quietly wider than the thread's runtime mode.
func TestNormalizeClaudePermissionModeAcceptsEverySelectableMode(t *testing.T) {
	for _, mode := range provider.AllRuntimeModes {
		base := claudeBasePermissionMode(mode)
		if got := normalizeClaudePermissionMode(base); got != base {
			t.Errorf("runtime mode %q maps to %q but normalizeClaudePermissionMode returns %q", mode, base, got)
		}
	}
}
