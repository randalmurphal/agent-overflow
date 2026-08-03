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
		{provider.EffortMax, "max"},
		{provider.EffortUltra, "ultra"},
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

// wantCodexRuntime is THE expected RuntimeMode → codex wire mapping. One table,
// asserted from two angles below: TestRuntimeModeToCodex checks each entry is
// right, TestRuntimeModeToCodexCoversEveryMode checks the table has an entry
// for every canonical tier. Keeping it in one place is what stops the two
// checks from disagreeing about what "correct" means.
//
// Notes on the entries that look duplicated but are not:
//   - read-only and approval-required share the OS sandbox but differ on
//     escalation: read-only never prompts (unattended), approval-required
//     escalates every non-read command to the human who is present.
//   - approval-required and auto share the policy/sandbox pair entirely and
//     differ only in reviewer. That is the definition of the auto tier: same
//     set of escalations, different answerer. Widening the sandbox would take
//     workspace writes out of the reviewer's jurisdiction while the tier's
//     label still promised review of each sensitive tool use.
//   - read-only and full-access name a reviewer even though neither ever
//     raises an approval request. The reviewer is thread state Codex keeps
//     until something overwrites it, so every tier states it explicitly rather
//     than letting a previous mode's choice survive the switch.
var wantCodexRuntime = map[provider.RuntimeMode]codexRuntime{
	provider.RuntimeReadOnly:         {ApprovalPolicy: "never", Sandbox: "read-only", ApprovalsReviewer: "user"},
	provider.RuntimeApprovalRequired: {ApprovalPolicy: "untrusted", Sandbox: "read-only", ApprovalsReviewer: "user"},
	provider.RuntimeAutoAcceptEdits:  {ApprovalPolicy: "on-request", Sandbox: "workspace-write", ApprovalsReviewer: "user"},
	provider.RuntimeAuto:             {ApprovalPolicy: "untrusted", Sandbox: "read-only", ApprovalsReviewer: "auto_review"},
	provider.RuntimeFullAccess:       {ApprovalPolicy: "never", Sandbox: "danger-full-access", ApprovalsReviewer: "user"},
}

// TestRuntimeModeToCodex enumerates every runtime tier and asserts the
// (approval, sandbox, reviewer) triple each produces. Split-then-compose means
// a future RuntimeMode addition without touching every helper is caught here —
// the exhaustiveness guard below turns "forgot a mode" into a failure rather
// than a silent fall-through to the untrusted/read-only/user default.
func TestRuntimeModeToCodex(t *testing.T) {
	for mode, want := range wantCodexRuntime {
		t.Run(string(mode), func(t *testing.T) {
			got := runtimeModeToCodex(mode)
			if got != want {
				t.Errorf("runtimeModeToCodex(%q) = %+v, want %+v", mode, got, want)
			}
		})
	}
}

// TestOnlyAutoRoutesApprovalsToTheReviewer states the tier boundary as its own
// property rather than leaving it implicit in the table: exactly one runtime
// mode hands approvals to Codex's reviewer subagent. A second tier drifting
// onto auto_review would start billing reviewer turns for a mode whose picker
// copy makes no such promise.
func TestOnlyAutoRoutesApprovalsToTheReviewer(t *testing.T) {
	for _, mode := range provider.AllRuntimeModes {
		want := approvalsReviewerUser
		if mode == provider.RuntimeAuto {
			want = approvalsReviewerAuto
		}
		if got := codexApprovalsReviewer(mode); got != want {
			t.Errorf("codexApprovalsReviewer(%q) = %q, want %q", mode, got, want)
		}
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

// TestConfigFromOptionsRuntimeModesTriple confirms every tier writes
// ApprovalPolicy, Sandbox, and ApprovalsReviewer in lockstep. This is the
// integration-y check that glues ConfigFromOptions to the canonical
// RuntimeMode → codex helpers: the helpers can be individually right and the
// Config still be wrong if one of the three is dropped on the way across.
func TestConfigFromOptionsRuntimeModesTriple(t *testing.T) {
	for _, mode := range provider.AllRuntimeModes {
		want, ok := wantCodexRuntime[mode]
		if !ok {
			t.Fatalf("runtime mode %q missing from wantCodexRuntime", mode)
		}
		t.Run(string(mode), func(t *testing.T) {
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider:    "codex",
				RuntimeMode: mode,
			})
			got := codexRuntime{
				ApprovalPolicy:    cfg.ApprovalPolicy,
				Sandbox:           cfg.Sandbox,
				ApprovalsReviewer: cfg.ApprovalsReviewer,
			}
			if got != want {
				t.Errorf("ConfigFromOptions runtime triple = %+v, want %+v", got, want)
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
	if cfg.ServiceTier != "priority" {
		t.Errorf("ServiceTier = %q, want priority", cfg.ServiceTier)
	}
}

// TestConfigFromOptionsServiceTierComesFromTheModelTier covers the axis as a
// SEQUENCE, not a set of states: fast mode is a toggle and the tier id is
// resolved per model, so the pairs that matter are the transitions. A tier id
// left behind after a toggle-off, or a stale id surviving a model switch, would
// pass a states-only test.
func TestConfigFromOptionsServiceTierComesFromTheModelTier(t *testing.T) {
	tests := []struct {
		name     string
		fastMode bool
		tierID   string
		want     string
	}{
		{name: "off ignores the tier id entirely", fastMode: false, tierID: "turbo", want: ""},
		{name: "on sends the catalog tier id", fastMode: true, tierID: "turbo", want: "turbo"},
		{name: "on with an unresolved model falls back to the legacy id", fastMode: true, tierID: "", want: fastServiceTier},
		{name: "on with a blank-ish tier id falls back too", fastMode: true, tierID: "   ", want: fastServiceTier},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider:       "codex",
				Model:          "gpt-5.5",
				FastMode:       tt.fastMode,
				FastModeTierID: tt.tierID,
			})
			if cfg.ServiceTier != tt.want {
				t.Fatalf("ServiceTier = %q, want %q", cfg.ServiceTier, tt.want)
			}
		})
	}

	base := provider.SessionOptions{Provider: "codex", Model: "gpt-5.5", FastModeTierID: "turbo"}

	on := base
	on.FastMode = true
	if got := ConfigFromOptions(on).ServiceTier; got != "turbo" {
		t.Fatalf("off→on ServiceTier = %q, want turbo", got)
	}
	off := on
	off.FastMode = false
	if got := ConfigFromOptions(off).ServiceTier; got != "" {
		t.Fatalf("on→off ServiceTier = %q, want the key omitted", got)
	}
	backOn := off
	backOn.FastMode = true
	backOn.FastModeTierID = "priority"
	if got := ConfigFromOptions(backOn).ServiceTier; got != "priority" {
		t.Fatalf("off→on after a model switch ServiceTier = %q, want priority", got)
	}
}

func TestBuildThreadParamsThreadsServiceTier(t *testing.T) {
	params := buildThreadParams(Config{ServiceTier: "priority"})
	if params["serviceTier"] != "priority" {
		t.Errorf("serviceTier = %v, want priority", params["serviceTier"])
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

func TestConfigFromOptionsTrustsValidatedFastModeForLiveOnlyModel(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider: "codex",
		Model:    "gpt-live-only",
		FastMode: true,
	})
	if cfg.ServiceTier != fastServiceTier {
		t.Errorf("ServiceTier = %q, want %q for app-validated live model", cfg.ServiceTier, fastServiceTier)
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

// TestRuntimeModeToCodexCoversEveryMode makes the mapping table above
// exhaustive by construction. Without it, a new RuntimeMode silently takes
// the default branch (untrusted/read-only) — which for an unattended mode
// means every command escalates to a human who is not there.
func TestRuntimeModeToCodexCoversEveryMode(t *testing.T) {
	if len(wantCodexRuntime) != len(provider.AllRuntimeModes) {
		t.Fatalf("wantCodexRuntime has %d entries, provider.AllRuntimeModes has %d — the table has a mode the canonical list does not",
			len(wantCodexRuntime), len(provider.AllRuntimeModes))
	}
	for _, mode := range provider.AllRuntimeModes {
		want, ok := wantCodexRuntime[mode]
		if !ok {
			t.Fatalf("runtime mode %q has no asserted codex mapping — add one here and in runtimeModeToCodex", mode)
		}
		if got := runtimeModeToCodex(mode); got != want {
			t.Errorf("runtimeModeToCodex(%q) = %+v, want %+v", mode, got, want)
		}
	}
}

// TestReadOnlySandboxIsAcceptedByThreadAndTurnParams proves the sandbox
// string the read-only mode produces survives both wire paths: the
// thread/start normalizer and the per-turn override builder. A sandbox value
// the turn builder rejects would fail every runtime-mode change mid-session.
func TestReadOnlySandboxIsAcceptedByThreadAndTurnParams(t *testing.T) {
	sandbox := runtimeModeToCodex(provider.RuntimeReadOnly).Sandbox
	if got := normalizeThreadSandbox(sandbox); got != sandbox {
		t.Errorf("normalizeThreadSandbox(%q) = %q — read-only must survive verbatim", sandbox, got)
	}
	policy, err := turnSandboxPolicy(sandbox)
	if err != nil {
		t.Fatalf("turnSandboxPolicy(%q): %v", sandbox, err)
	}
	if policy["type"] != "readOnly" {
		t.Errorf("turnSandboxPolicy(%q) = %v, want type readOnly", sandbox, policy)
	}
}
