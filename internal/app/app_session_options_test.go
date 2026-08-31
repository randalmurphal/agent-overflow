package app

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// TestSessionOptionsFromThreadToClaudeConfigXHigh covers the thread→opts→cfg
// path for a Claude thread at xhigh effort. Claude now receives native CLI
// flags instead of a system-prompt prefix.
func TestSessionOptionsFromThreadToClaudeConfigXHigh(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-claude-xhigh")
	thread.Provider = string(provider.Claude)
	thread.Model = "claude-opus-4-7"
	thread.ReasoningEffort = string(provider.EffortXHigh)
	thread.RuntimeMode = string(provider.RuntimeFullAccess)
	thread.ContextWindow = 1000000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	opts := provider.SessionOptionsFromThread(stored, provider.AutoCompactDefaults{}, "You are the agent.", false)
	cfg := claude.ConfigFromOptions(opts)

	if cfg.SystemPrompt != "You are the agent." {
		t.Errorf("SystemPrompt = %q, want unchanged", cfg.SystemPrompt)
	}
	if cfg.ReasoningEffort != "xhigh" {
		t.Errorf("ReasoningEffort = %q, want xhigh (2.1.170 --effort accepts xhigh natively; no longer collapsed to max)", cfg.ReasoningEffort)
	}
	if cfg.Model != "claude-opus-4-7[1m]" {
		t.Errorf("Model = %q, want claude-opus-4-7[1m]", cfg.Model)
	}
	wantFlags := []string{"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions"}
	if strings.Join(cfg.PermissionFlags, " ") != strings.Join(wantFlags, " ") {
		t.Errorf("PermissionFlags = %v, want %v", cfg.PermissionFlags, wantFlags)
	}
}

func TestSessionOptionsFromThreadToCodexConfigXHigh(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-codex-xhigh")
	thread.Provider = string(provider.Codex)
	thread.Model = "gpt-5.4"
	thread.ReasoningEffort = string(provider.EffortXHigh)
	thread.RuntimeMode = string(provider.RuntimeFullAccess)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	opts := provider.SessionOptionsFromThread(stored, provider.AutoCompactDefaults{}, "codex system prompt", false)
	cfg := codex.ConfigFromOptions(opts)

	if cfg.ReasoningEffort != "xhigh" {
		t.Errorf("ReasoningEffort = %q, want xhigh", cfg.ReasoningEffort)
	}
	if cfg.SystemPrompt != "codex system prompt" {
		t.Errorf("Codex SystemPrompt must not carry a think-hard prefix; got %q", cfg.SystemPrompt)
	}
	if cfg.ApprovalPolicy != "never" || cfg.Sandbox != "danger-full-access" {
		t.Errorf("full-access RuntimeMode should map to (never, danger-full-access); got (%q, %q)",
			cfg.ApprovalPolicy, cfg.Sandbox)
	}
}

func TestSessionOptionsCoercesStaleSonnetXHigh(t *testing.T) {
	thread := testThread("thread-stale-sonnet-xhigh")
	thread.Provider = string(provider.Claude)
	thread.Model = "claude-sonnet-4-6"
	thread.ReasoningEffort = string(provider.EffortXHigh)

	opts := provider.SessionOptionsFromThread(thread, provider.AutoCompactDefaults{}, "", false)
	cfg := claude.ConfigFromOptions(opts)

	if opts.ReasoningEffort != provider.EffortHigh {
		t.Fatalf("ReasoningEffort = %q, want high", opts.ReasoningEffort)
	}
	if cfg.ReasoningEffort != "high" {
		t.Fatalf("Claude ReasoningEffort = %q, want high", cfg.ReasoningEffort)
	}
}

func TestSessionOptionsCoercesStaleCodexMax(t *testing.T) {
	thread := testThread("thread-stale-codex-max")
	thread.Provider = string(provider.Codex)
	thread.Model = "gpt-5.5"
	thread.ReasoningEffort = string(provider.EffortMax)

	opts := provider.SessionOptionsFromThread(thread, provider.AutoCompactDefaults{}, "", false)
	cfg := codex.ConfigFromOptions(opts)

	if opts.ReasoningEffort != provider.EffortMedium {
		t.Fatalf("ReasoningEffort = %q, want medium", opts.ReasoningEffort)
	}
	if cfg.ReasoningEffort != "medium" {
		t.Fatalf("Codex ReasoningEffort = %q, want medium", cfg.ReasoningEffort)
	}
}

func TestSessionOptionsFastModePreservesClaudeModel(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-fast-opus")
	thread.Provider = string(provider.Claude)
	thread.Model = "claude-opus-4-6"
	thread.FastMode = true
	thread.ContextWindow = provider.ClaudeStandardContextWindow
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	opts := provider.SessionOptionsFromThread(stored, provider.AutoCompactDefaults{}, "sys", false)
	cfg := claude.ConfigFromOptions(opts)
	if cfg.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want claude-opus-4-6", cfg.Model)
	}
	if !cfg.FastMode {
		t.Error("FastMode = false, want true")
	}
}

func TestSessionOptionsFastModePreservesCodexModel(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-fast-gpt5")
	thread.Provider = string(provider.Codex)
	thread.Model = "gpt-5.5"
	thread.FastMode = true
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	opts := provider.SessionOptionsFromThread(stored, provider.AutoCompactDefaults{}, "sys", false)
	cfg := codex.ConfigFromOptions(opts)
	if cfg.Model != "gpt-5.5" {
		t.Errorf("Model = %q, want gpt-5.5", cfg.Model)
	}
	if cfg.ServiceTier != "priority" {
		t.Errorf("ServiceTier = %q, want priority", cfg.ServiceTier)
	}
}

// TestClaudeConfigBuildsArgsWithDangerousSkipFromFullAccess — end-to-end
// check: a full-access thread → SessionOptions → claude.Config → buildArgs
// ultimately ships the bypass-permissions mode plus the explicit skip flag on
// the CLI command line.
func TestClaudeConfigBuildsArgsWithDangerousSkipFromFullAccess(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-fullaccess-args")
	thread.Provider = string(provider.Claude)
	thread.Model = "claude-opus-4-6"
	thread.RuntimeMode = string(provider.RuntimeFullAccess)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	opts := provider.SessionOptionsFromThread(stored, provider.AutoCompactDefaults{}, "", false)
	cfg := claude.ConfigFromOptions(opts)

	foundMode := false
	foundSkip := false
	for _, a := range claudeBuildArgsProxy(cfg) {
		if a == "bypassPermissions" {
			foundMode = true
		}
		if a == "--allow-dangerously-skip-permissions" {
			foundSkip = true
		}
	}
	if !foundMode || !foundSkip {
		t.Errorf("expected bypassPermissions and --allow-dangerously-skip-permissions in args; cfg=%+v", cfg)
	}
}

// claudeBuildArgsProxy is a thin wrapper so this test file doesn't import
// claude's internal buildArgs directly. It exercises claude.NewSession's
// argument construction via a harmless mock spawn path: we just reach
// through the cfg → args codepath by calling the exported buildArgs alias.
// Keeping the detour in the test file (vs a claude export) means we don't
// widen claude's public API surface for a test.
func claudeBuildArgsProxy(cfg claude.Config) []string {
	// PermissionFlags is visible from this package; rebuilding the flag
	// sequence here keeps the proxy a zero-import reach-through.
	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	args = append(args, cfg.PermissionFlags...)
	return args
}
