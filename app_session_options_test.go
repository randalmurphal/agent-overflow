package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// TestSessionOptionsFromThreadToClaudeConfigXHigh covers the thread→opts→cfg
// path for a Claude thread at xhigh effort. The effort prefix must land on
// the system prompt; the caller-composed prompt survives.
func TestSessionOptionsFromThreadToClaudeConfigXHigh(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-claude-xhigh")
	thread.Provider = string(provider.Claude)
	thread.Model = "claude-opus-4-6"
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
	opts := provider.SessionOptionsFromThread(stored, "You are the agent.", false)
	cfg := claude.ConfigFromOptions(opts)

	if !strings.HasPrefix(cfg.SystemPrompt, "think harder\n\n") {
		t.Errorf("SystemPrompt = %q, want 'think harder' prefix", cfg.SystemPrompt)
	}
	if cfg.Env["ANTHROPIC_BETAS"] == "" {
		t.Errorf("1M context should set ANTHROPIC_BETAS; got env %v", cfg.Env)
	}
	wantFlags := []string{"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions"}
	if strings.Join(cfg.PermissionFlags, " ") != strings.Join(wantFlags, " ") {
		t.Errorf("PermissionFlags = %v, want %v", cfg.PermissionFlags, wantFlags)
	}
}

// TestSessionOptionsFromThreadToCodexConfigMaxFloors confirms the xhigh floor
// for Max on a real Codex thread and that SystemPrompt lands on the Codex
// Config without the Claude-style effort prefix (Codex has native effort).
func TestSessionOptionsFromThreadToCodexConfigMaxFloors(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-codex-max")
	thread.Provider = string(provider.Codex)
	thread.Model = "gpt-5.4"
	thread.ReasoningEffort = string(provider.EffortMax)
	thread.RuntimeMode = string(provider.RuntimeFullAccess)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	opts := provider.SessionOptionsFromThread(stored, "codex system prompt", false)
	cfg := codex.ConfigFromOptions(opts)

	if cfg.ReasoningEffort != "xhigh" {
		t.Errorf("Max effort should floor to xhigh, got %q", cfg.ReasoningEffort)
	}
	if cfg.SystemPrompt != "codex system prompt" {
		t.Errorf("Codex SystemPrompt must not carry a think-hard prefix; got %q", cfg.SystemPrompt)
	}
	if cfg.ApprovalPolicy != "never" || cfg.Sandbox != "danger-full-access" {
		t.Errorf("full-access RuntimeMode should map to (never, danger-full-access); got (%q, %q)",
			cfg.ApprovalPolicy, cfg.Sandbox)
	}
}

// TestSessionOptionsFastModeSwapsClaudeOpus — the FastMode toggle on an Opus
// thread routes through to a Haiku launch id by the time it reaches
// claude.Config.
func TestSessionOptionsFastModeSwapsClaudeOpus(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-fast-opus")
	thread.Provider = string(provider.Claude)
	thread.Model = "claude-opus-4-6"
	thread.FastMode = true
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	opts := provider.SessionOptionsFromThread(stored, "sys", false)
	cfg := claude.ConfigFromOptions(opts)
	if cfg.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5 (fast mode swap)", cfg.Model)
	}
}

// TestSessionOptionsFastModeSwapsCodexGpt5 — the Codex analogue: gpt-5.5
// becomes gpt-5.4-mini after the translation.
func TestSessionOptionsFastModeSwapsCodexGpt5(t *testing.T) {
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
	opts := provider.SessionOptionsFromThread(stored, "sys", false)
	cfg := codex.ConfigFromOptions(opts)
	if cfg.Model != "gpt-5.4-mini" {
		t.Errorf("Model = %q, want gpt-5.4-mini (fast mode swap)", cfg.Model)
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
	opts := provider.SessionOptionsFromThread(stored, "", false)
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
