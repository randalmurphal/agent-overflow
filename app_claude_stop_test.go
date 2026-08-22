package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// TestStopClaudeTask_SessionMissing covers the first input-validation
// branch: a caller that stops a task before Start / after Close must
// see a clear error, not a silent no-op.
func TestStopClaudeTask_SessionMissing(t *testing.T) {
	a := NewApp()

	err := a.StopClaudeTask("no-such-thread", "task-123")
	if err == nil {
		t.Fatal("StopClaudeTask with no session: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("error should mention missing session, got: %v", err)
	}
}

// TestStopClaudeTask_ProviderMismatch covers the Codex-thread branch:
// Codex's per-row stop is TerminateCodexBackgroundTerminal, keyed by PTY
// process id rather than task id, so a caller who reached the Claude
// stop binding for a Codex thread is programming against the wrong API
// and must see a loud error. Silently dropping would leave the UI's
// per-row Stop button broken on Codex threads with no indication why.
func TestStopClaudeTask_ProviderMismatch(t *testing.T) {
	a := NewApp()

	// Install a session entry whose provider-level typed fields are
	// both nil — mirrors a Codex session for the narrow contract this
	// test probes (the binding only cares that sess.claude is nil).
	// We could install a real codex.Session, but that would drag in
	// the whole probe / RPC harness without exercising anything this
	// test needs.
	a.mu.Lock()
	a.sessions["codex-thread"] = session{provider: "codex"}
	a.mu.Unlock()

	err := a.StopClaudeTask("codex-thread", "task-123")
	if err == nil {
		t.Fatal("StopClaudeTask on non-Claude thread: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "is not a Claude thread") {
		t.Errorf("error should mention provider mismatch, got: %v", err)
	}
}

// TestStopClaudeTask_RoundTripSucceeds drives the happy path: a real
// claude.Session wired to a fake-CLI script that answers stop_task
// control_requests. Confirms the binding's context + session lookup
// + StopTask glue all work end-to-end — the session-level test
// (TestSession_StopTask_SuccessRoundTrip) already proves the wire
// shape; this one proves the binding hooks through correctly.
func TestStopClaudeTask_RoundTripSucceeds(t *testing.T) {
	a := NewApp()

	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/fake-claude"
	// Same script shape as the provider-level stop_task success test:
	// read stdin, match stop_task lines, echo a subtype=success
	// control_response with the matching request_id.
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"stop_task"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sess, err := claude.NewSession(ctx, "claude-thread", claude.Config{
		Binary: scriptPath,
	}, func(_ provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	a.mu.Lock()
	a.sessions["claude-thread"] = session{provider: "claude", claude: sess}
	a.mu.Unlock()

	if err := a.StopClaudeTask("claude-thread", "task-abc"); err != nil {
		t.Fatalf("StopClaudeTask: %v", err)
	}
}

// TestStopClaudeTask_ShuttingDown short-circuits the binding when the
// App is mid-teardown. This mirrors every other binding entry point
// (RespondToApproval, SendMessage, ...) that fails fast with
// ErrShuttingDown rather than racing against a subsystem that's
// being torn down.
func TestStopClaudeTask_ShuttingDown(t *testing.T) {
	a := NewApp()
	a.shuttingDown.Store(true)

	err := a.StopClaudeTask("any-thread", "task-123")
	if err != ErrShuttingDown {
		t.Fatalf("err = %v, want ErrShuttingDown", err)
	}
}

// TestBackgroundClaudeTask_SessionMissing mirrors the StopClaudeTask
// guard: backgrounding before Start / after Close must be a clear
// error, not a silent no-op that leaves the row's button dead.
func TestBackgroundClaudeTask_SessionMissing(t *testing.T) {
	a := NewApp()

	err := a.BackgroundClaudeTask("no-such-thread", "toolu_123")
	if err == nil {
		t.Fatal("BackgroundClaudeTask with no session: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("error should mention missing session, got: %v", err)
	}
}

// TestBackgroundClaudeTask_ProviderMismatch: Codex has no equivalent —
// a spawned collab-agent child is already asynchronous and close_agent
// is a model-only tool — so a caller who reached this binding for a
// Codex thread is programming against the wrong API and must see it.
func TestBackgroundClaudeTask_ProviderMismatch(t *testing.T) {
	a := NewApp()

	a.mu.Lock()
	a.sessions["codex-thread"] = session{provider: "codex"}
	a.mu.Unlock()

	err := a.BackgroundClaudeTask("codex-thread", "toolu_123")
	if err == nil {
		t.Fatal("BackgroundClaudeTask on non-Claude thread: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "is not a Claude thread") {
		t.Errorf("error should mention provider mismatch, got: %v", err)
	}
}

// TestBackgroundClaudeTask_RoundTripSucceeds drives the happy path
// through a real claude.Session wired to a fake CLI that answers the
// background_tasks control_request with `{backgrounded:true}`. The
// session-level tests own the wire shape; this proves the binding's
// context + lookup + BackgroundTask glue hooks through.
func TestBackgroundClaudeTask_RoundTripSucceeds(t *testing.T) {
	a := NewApp()

	scriptPath := t.TempDir() + "/fake-claude"
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"background_tasks"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"backgrounded":true}}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sess, err := claude.NewSession(ctx, "claude-thread", claude.Config{
		Binary: scriptPath,
	}, func(_ provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	a.mu.Lock()
	a.sessions["claude-thread"] = session{provider: "claude", claude: sess}
	a.mu.Unlock()

	if err := a.BackgroundClaudeTask("claude-thread", "toolu_abc"); err != nil {
		t.Fatalf("BackgroundClaudeTask: %v", err)
	}
}

// TestBackgroundClaudeTask_ShuttingDown short-circuits mid-teardown like
// every other binding entry point.
func TestBackgroundClaudeTask_ShuttingDown(t *testing.T) {
	a := NewApp()
	a.shuttingDown.Store(true)

	err := a.BackgroundClaudeTask("any-thread", "toolu_123")
	if err != ErrShuttingDown {
		t.Fatalf("err = %v, want ErrShuttingDown", err)
	}
}
