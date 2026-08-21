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
	// The marker is a context TIER, and the thread's ContextWindow is the only
	// thing that decides it. An id that arrives already carrying one (the CLI's
	// own model list bakes them in) must not double up, and must not smuggle
	// the extended tier past a standard-window thread.
	if got := claudeModelForContextWindow("claude-opus-4-7[1m]", provider.ClaudeExtendedContextWindow); got != "claude-opus-4-7[1m]" {
		t.Fatalf("pre-marked extended model = %q, want a single [1m] suffix", got)
	}
	if got := claudeModelForContextWindow("claude-opus-4-7[1m]", provider.ClaudeStandardContextWindow); got != "claude-opus-4-7" {
		t.Fatalf("pre-marked standard model = %q, want the marker dropped", got)
	}
}

func TestConfigFromOptionsFastModePreservesModelAndSetsFlag(t *testing.T) {
	for _, model := range []string{"claude-opus-4-7", "claude-opus-4-6"} {
		t.Run(model, func(t *testing.T) {
			// Standard tier is stated, not left to the catalog default:
			// the assertion below is "fast mode leaves the model string
			// alone", and only the extended tier appends `[1m]`.
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider:      "claude",
				Model:         model,
				ContextWindow: provider.ClaudeStandardContextWindow,
				FastMode:      true,
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

// TestConfigFromOptionsUnsetContextWindowUsesModelDefault pins the one place
// the registry's Default context-window flag reaches the wire: an unset
// SessionOptions.ContextWindow resolves through
// provider.ResolveContextWindowForModel, so the large models spawn on the 1M
// tier (`[1m]` model suffix) while Sonnet keeps 1M opt-in.
func TestConfigFromOptionsUnsetContextWindowUsesModelDefault(t *testing.T) {
	cases := []struct {
		model     string
		wantModel string
		wantTier  int
	}{
		{"claude-fable-5", "claude-fable-5[1m]", provider.ClaudeExtendedContextWindow},
		{"claude-opus-5", "claude-opus-5[1m]", provider.ClaudeExtendedContextWindow},
		{"claude-opus-4-7", "claude-opus-4-7[1m]", provider.ClaudeExtendedContextWindow},
		{"claude-sonnet-5", "claude-sonnet-5", provider.ClaudeStandardContextWindow},
		{"claude-sonnet-4-6", "claude-sonnet-4-6", provider.ClaudeStandardContextWindow},
		{"claude-haiku-4-5", "claude-haiku-4-5", provider.ClaudeStandardContextWindow},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider: "claude",
				Model:    tc.model,
			})
			if cfg.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", cfg.Model, tc.wantModel)
			}
			if cfg.ContextWindow != tc.wantTier {
				t.Errorf("ContextWindow = %d, want %d", cfg.ContextWindow, tc.wantTier)
			}
		})
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
		ContextWindow:              provider.ClaudeStandardContextWindow,
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
		ContextWindow:              provider.ClaudeStandardContextWindow,
		AutoCompactStandardPercent: 50,
	})
	args := buildArgs(cfg, "")
	joined := strings.Join(args, " ")
	want := `--settings {"crossSessionInbound":"refuse","env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"50","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"200000"}}`
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
	args := buildArgs(cfg, "")
	joined := strings.Join(args, " ")
	want := `--settings {"crossSessionInbound":"refuse","env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"40","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"1000000"}}`
	if !strings.Contains(joined, want) {
		t.Fatalf("args missing extended-tier autocompact settings: got=%v\nwant substring=%q", args, want)
	}
}

// TestBuildArgsAutoCompactOmitsWindowWhenUnresolved covers hand-built
// Configs that bypass ConfigFromOptions: a percent with no known window
// still renders the pct override but must not invent a window value.
func TestBuildArgsAutoCompactOmitsWindowWhenUnresolved(t *testing.T) {
	cfg := Config{AutoCompactPercent: 50}
	args := buildArgs(cfg, "")
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
		ContextWindow:              provider.ClaudeStandardContextWindow,
		FastMode:                   true,
		AutoCompactStandardPercent: 50,
	})
	args := buildArgs(cfg, "")
	joined := strings.Join(args, " ")
	want := `--settings {"fastMode":true,"crossSessionInbound":"refuse","env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"50","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"200000"}}`
	if !strings.Contains(joined, want) {
		t.Fatalf("args missing combined fastMode + autocompact settings: got=%v\nwant substring=%q", args, want)
	}
	if got := strings.Count(joined, "--settings"); got != 1 {
		t.Fatalf("expected exactly one --settings flag, got %d in %v", got, args)
	}
}

// TestBuildArgsOmitsSettingsFlagWhenNothingSet pins the "zero value means
// say nothing" contract for the whole block: an unset axis renders no
// key, and a block with no keys renders no flag rather than an inert
// `{}`. The Config is hand-stamped because ConfigFromOptions no longer
// produces an empty block — see the cross-session refusal below.
func TestBuildArgsOmitsSettingsFlagWhenNothingSet(t *testing.T) {
	args := buildArgs(Config{Model: "claude-opus-4-7"}, "")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--settings") {
		t.Fatalf("expected no --settings flag when no settings-block axis is set, got %v", args)
	}
}

// TestBuildArgsAlwaysStatesTheCrossSessionRefusal is the one axis that
// breaks the "say nothing when unset" rule, deliberately.
//
// Silence is not off here. AO's own gate is CLAUDE_CODE_HARBOR_KITE, but
// the CLI can also bind the inbox from the remote GrowthBook flag
// `tengu_harbor_kite`, and `tengu_harbor_kite_mode_emit` — which has no
// env override at all — turns on the permission-class attestation that
// the CLI's mode-parity default reads. With both remote flags live and
// the key absent, a peer whose permission class matches this session
// would AUTO-DELIVER: a turn started in a thread whose user never
// enabled the feature. So every spawn states the refusal, and the flag
// is now present on every Claude session as a result.
func TestBuildArgsAlwaysStatesTheCrossSessionRefusal(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "claude",
		Model:    "claude-opus-4-7",
	})
	joined := strings.Join(buildArgs(cfg, ""), " ")
	if !strings.Contains(joined, `--settings {"crossSessionInbound":"refuse"}`) {
		t.Fatalf("a cross-session-disabled spawn must state the refusal, got %s", joined)
	}
}

// TestCrossSessionOptionsRenderGateNameAndPolicy covers the whole spawn
// surface of the peer inbox, driven from SessionOptions the way the app
// drives it — the three pieces have to agree or the feature half-works:
// the gate variable binds the socket, `--name` makes the session
// addressable as something other than its cwd basename, and
// `crossSessionInbound` decides what an arriving message does.
//
// "hold" is deliberately absent from the policies exercised here. It is a
// legal CLI value and an illegal Agent Overflow one — it parks a message
// with no approval surface a headless session can present — and
// internal/settings refuses it at the save (claudecrosssession.go).
func TestCrossSessionOptionsRenderGateNameAndPolicy(t *testing.T) {
	for _, policy := range []string{"accept", "refuse"} {
		cfg := ConfigFromOptions(provider.SessionOptions{
			Provider:              "claude",
			Model:                 "claude-opus-4-7",
			ClaudeCrossSession:    provider.ClaudeCrossSession{Enabled: true, Inbound: policy},
			ClaudePeerSessionName: "AO Thread One",
		})
		cfg.PeerSessionName = "AO Thread One"
		joined := strings.Join(buildArgs(cfg, ""), " ")
		want := `--settings {"crossSessionInbound":"` + policy + `"}`
		if !strings.Contains(joined, want) {
			t.Fatalf("policy %q: got=%s\nwant substring=%q", policy, joined, want)
		}
		if !strings.Contains(joined, "--name AO Thread One") {
			t.Fatalf("policy %q: missing --name: %s", policy, joined)
		}
		env := withClaudeCrossSessionEnv(nil, cfg)
		if env[claudeHarborKiteEnv] != "1" {
			t.Fatalf("policy %q: gate variable = %q, want \"1\"", policy, env[claudeHarborKiteEnv])
		}
	}
}

// Off states the refusal and nothing else. The gate variable is ABSENT
// rather than "0" — the CLI's override is checked for truthiness and
// falls through to the remote flag when unset, so no env value can mean
// "off" and only the settings key can. `--name` is dropped with it: a
// name AO derived for peer addressing has no business overriding the
// /resume picker's label for a session no peer can reach.
func TestCrossSessionOptionsDisabledEmitOnlyTheRefusal(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:              "claude",
		Model:                 "claude-opus-4-7",
		ClaudePeerSessionName: "AO Thread One",
	})
	cfg.PeerSessionName = "AO Thread One"
	joined := strings.Join(buildArgs(cfg, ""), " ")
	if !strings.Contains(joined, `"crossSessionInbound":"refuse"`) {
		t.Fatalf("disabled cross-session must still refuse: %s", joined)
	}
	if strings.Contains(joined, "--name") {
		t.Fatalf("disabled cross-session rendered --name: %s", joined)
	}
	if env := withClaudeCrossSessionEnv(map[string]string{"KEEP": "1"}, cfg); env[claudeHarborKiteEnv] != "" {
		t.Fatalf("disabled cross-session set the gate variable: %v", env)
	}
}

// An empty (or whitespace-only) name drops the flag rather than passing
// one the CLI reads as absent — and the inbox still binds, because the
// name is a label and the gate is the feature.
func TestCrossSessionNameFlagDroppedWhenNameSanitizesEmpty(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:           "claude",
		Model:              "claude-opus-4-7",
		ClaudeCrossSession: provider.ClaudeCrossSession{Enabled: true, Inbound: "accept"},
	})
	cfg.PeerSessionName = "   \u200b  "
	joined := strings.Join(buildArgs(cfg, ""), " ")
	if strings.Contains(joined, "--name") {
		t.Fatalf("empty name rendered a flag: %s", joined)
	}
	if !strings.Contains(joined, "crossSessionInbound") {
		t.Fatalf("missing policy without a name: %s", joined)
	}
}

// TestBuildArgsRendersOutputStyleIntoSettings covers the Claude-only
// output-style axis. It has no CLI flag — the `outputStyle` settings key
// is the only delivery path — and an empty value must leave the CLI's own
// resolution untouched rather than pinning "default".
func TestBuildArgsRendersOutputStyleIntoSettings(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{Provider: "claude", Model: "claude-opus-4-7"})
	cfg.OutputStyle = "Explanatory"
	joined := strings.Join(buildArgs(cfg, ""), " ")
	want := `--settings {"crossSessionInbound":"refuse","outputStyle":"Explanatory"}`
	if !strings.Contains(joined, want) {
		t.Fatalf("args missing output style: got=%s\nwant substring=%q", joined, want)
	}

	cfg.OutputStyle = "   "
	joined = strings.Join(buildArgs(cfg, ""), " ")
	if strings.Contains(joined, "outputStyle") {
		t.Fatalf("blank output style must not render a key: %s", joined)
	}
}

// TestBuildArgsRendersSubagentAndMemoryEnv pins the three env-backed axes.
// They ride the settings block's `env` map rather than the subprocess
// environment because Claude reapplies its own settings.json env over
// inherited values at init — a value put in the real environment can be
// silently overwritten, one put here cannot.
func TestBuildArgsRendersSubagentAndMemoryEnv(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{Provider: "claude", Model: "claude-opus-4-7"})
	cfg.MaxSubagentSpawnDepth = 2
	cfg.MaxConcurrentSubagents = 6
	cfg.ToolMemoryLimit = "4G"
	joined := strings.Join(buildArgs(cfg, ""), " ")
	for _, want := range []string{
		`"CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH":"2"`,
		`"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS":"6"`,
		`"CLAUDE_CODE_TOOL_MEMORY_LIMIT":"4G"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %s: %s", want, joined)
		}
	}
}

// Zero is not a sendable value: the CLI's schema for both counters is
// `int({min:1, digitsOnly:true})`, so a rendered "0" would be rejected
// and the session would silently lose the user's other env values along
// with it.
func TestBuildArgsOmitsZeroSubagentLimits(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{Provider: "claude", Model: "claude-opus-4-7"})
	cfg.MaxSubagentSpawnDepth = 0
	cfg.MaxConcurrentSubagents = 0
	cfg.ToolMemoryLimit = "   "
	joined := strings.Join(buildArgs(cfg, ""), " ")
	for _, name := range []string{
		"CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH",
		"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS",
		"CLAUDE_CODE_TOOL_MEMORY_LIMIT",
	} {
		if strings.Contains(joined, name) {
			t.Fatalf("zero/blank axis leaked %s into --settings: %s", name, joined)
		}
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

// TestConfigFromOptionsRuntimeModeAuto pins the auto tier's launch config.
// The flag is the whole mapping: `--permission-mode auto` and nothing else.
//
//   - No `--allow-dangerously-skip-permissions`. Auto is not a bypass; the
//     classifier can DENY, and pairing the two would hand the session an
//     escape hatch the tier does not promise.
//   - No `--disallowedTools`. Auto's reviewer needs to SEE a write to rule on
//     it; removing the write tools would replace review with a blanket refusal
//     and (per PlanLiveUpdate) force a restart on every transition.
//
// Verified against claude 2.1.219: the CLI echoes `permissionMode:"auto"` on
// `system/init` for exactly this argv. See claudeAutoPermissionMode.
func TestConfigFromOptionsRuntimeModeAuto(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeAuto,
	})
	want := []string{"--permission-mode", "auto"}
	if !slices.Equal(cfg.PermissionFlags, want) {
		t.Errorf("PermissionFlags = %v, want %v", cfg.PermissionFlags, want)
	}
	if cfg.BasePermissionMode != "auto" {
		t.Errorf("BasePermissionMode = %q, want auto", cfg.BasePermissionMode)
	}
	if len(cfg.DisallowedTools) != 0 {
		t.Errorf("DisallowedTools = %v, want none for auto", cfg.DisallowedTools)
	}
	if slices.Contains(cfg.PermissionFlags, "--allow-dangerously-skip-permissions") {
		t.Error("auto must not carry --allow-dangerously-skip-permissions")
	}
}

// TestBuildArgsRendersAutoPermissionMode proves the auto mapping reaches argv.
// The flag is the entire enforcement mechanism for this tier, so a Config
// field the spawn path drops would silently start a supervised session that
// prompts for everything the mode promised to handle.
func TestBuildArgsRendersAutoPermissionMode(t *testing.T) {
	args := buildArgs(ConfigFromOptions(provider.SessionOptions{
		Provider:    "claude",
		RuntimeMode: provider.RuntimeAuto,
	}), "")
	idx := slices.Index(args, "auto")
	if idx <= 0 || args[idx-1] != "--permission-mode" {
		t.Errorf("args missing --permission-mode auto: %v", args)
	}
	// The CanUseTool responder stays installed: auto falls back to a real ask
	// on safety_check / ask_rule / plan_mode_floor, and an unanswered fallback
	// is a hung turn.
	promptIdx := slices.Index(args, "--permission-prompt-tool")
	if promptIdx < 0 || args[promptIdx+1] != "stdio" {
		t.Errorf("auto sessions must keep --permission-prompt-tool stdio: %v", args)
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
	args := buildArgs(cfg, "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model claude-opus-4-6[1m]") {
		t.Fatalf("args missing suffixed model: %v", args)
	}
	if !strings.Contains(joined, "--effort high") {
		t.Fatalf("args missing effort: %v", args)
	}
	if !strings.Contains(joined, `--settings {"fastMode":true,"crossSessionInbound":"refuse"}`) {
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
	args := buildArgs(cfg, "")
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
	args := buildArgs(cfg, "")
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
		provider.RuntimeAuto:             "auto",
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
	args := buildArgs(cfg, "")
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

// envLookup reads one variable out of a BuildEnvironment result, reporting
// presence separately from value — "absent" and "empty" are different
// answers here, and the gate's whole contract is about absence.
func envLookup(env []string, key string) (string, bool) {
	value, found := "", false
	for _, entry := range env {
		name, candidate, ok := strings.Cut(entry, "=")
		if ok && name == key {
			value, found = candidate, true
		}
	}
	return value, found
}

// B18: the gate is a variable the CHILD inherits, so "off" has to be
// produced rather than merely not-set. When Agent Overflow itself runs under
// CLAUDE_CODE_HARBOR_KITE=1 — a developer who exported it, or an AO started
// from inside a Claude session — a disabled setting used to leave cfg.Env
// untouched and the child inherited the gate: it bound the inbox and
// advertised itself in every peer's ListAgents while the UI said off.
// `crossSessionInbound: "refuse"` does not cover that; it blocks DELIVERY,
// not discovery.
func TestCrossSessionGateIsNotInheritedWhenDisabled(t *testing.T) {
	t.Setenv(claudeHarborKiteEnv, "1")
	t.Setenv(claudePeerSessionNameEnv, "someone-elses-name")

	cfg := ConfigFromOptions(provider.SessionOptions{Provider: "claude", Model: "claude-opus-4-7"})
	env := provider.BuildEnvironment(claudeSpawnEnv(cfg), claudeSpawnUnsetEnv()...)

	if value, found := envLookup(env, claudeHarborKiteEnv); found {
		t.Fatalf("%s = %q in the child environment, want it absent — the setting says off", claudeHarborKiteEnv, value)
	}
	// The name is AO's to state, in both directions: an inherited one would
	// register a thread under a label the app never chose.
	if value, found := envLookup(env, claudePeerSessionNameEnv); found {
		t.Fatalf("%s = %q in the child environment, want it absent", claudePeerSessionNameEnv, value)
	}
}

// The enabled direction over the same host environment: AO states the gate
// explicitly rather than relying on what it happened to inherit, so the value
// is exactly "1" no matter what the host exported.
func TestCrossSessionGateIsStatedExplicitlyWhenEnabled(t *testing.T) {
	t.Setenv(claudeHarborKiteEnv, "totally-bogus")
	t.Setenv(claudePeerSessionNameEnv, "someone-elses-name")

	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:           "claude",
		Model:              "claude-opus-4-7",
		ClaudeCrossSession: provider.ClaudeCrossSession{Enabled: true, Inbound: "accept"},
	})
	env := provider.BuildEnvironment(claudeSpawnEnv(cfg), claudeSpawnUnsetEnv()...)

	if value, found := envLookup(env, claudeHarborKiteEnv); !found || value != "1" {
		t.Fatalf("%s = %q (present=%v), want \"1\"", claudeHarborKiteEnv, value, found)
	}
	if value, found := envLookup(env, claudePeerSessionNameEnv); found {
		t.Fatalf("%s = %q, want it absent — AO passes the name as --name", claudePeerSessionNameEnv, value)
	}
}
