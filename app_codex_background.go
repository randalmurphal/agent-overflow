package main

import (
	"context"
	"fmt"
	"time"
)

// codexBackgroundTerminalStopTimeout bounds the Wails-side wait on both
// background-terminal stop RPCs (thread-wide clean and per-row
// terminate). The app-server terminates background PTYs in a single
// tick; the observable effect is a follow-up stream of item/completed
// events, not a slow response. 10s is a generous ceiling that still
// fails loudly if the subprocess wedges — mirrors the Claude stop-task
// binding's budget for the same reason.
const codexBackgroundTerminalStopTimeout = 10 * time.Second

// CleanCodexBackgroundTerminals asks the Codex app-server to terminate
// every running unified-exec background PTY for `threadID`. This is the
// thread-wide "Stop all" primitive for Codex; the per-row stop is
// TerminateCodexBackgroundTerminal below.
//
// After the RPC succeeds, Codex emits one `item/completed` notification
// per terminated PTY. Those update triage's transient tray state; the
// command output becomes transcript history only if the model explicitly
// waits/polls the terminal with write_stdin. No follow-up work is needed
// here.
//
// Returns typed errors for:
//
//   - session-missing: no Codex session for this thread. The caller
//     reached the binding before Start / after Close.
//   - provider-mismatch: the thread exists but it's a Claude session.
//     Claude has its own per-row stop primitive (StopClaudeTask); the
//     frontend must branch on provider before reaching for this.
//   - timeout / provider error: surfaced verbatim so the UI can render
//     the CLI-supplied message.
func (a *App) CleanCodexBackgroundTerminals(threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}

	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return fmt.Errorf("app: clean codex background terminals: no active session for thread %s", threadID)
	}
	if sess.codex == nil {
		return fmt.Errorf("app: clean codex background terminals: thread %s is not a Codex thread", threadID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexBackgroundTerminalStopTimeout)
	defer cancel()
	return sess.codex.CleanBackgroundTerminals(ctx)
}

// TerminateCodexBackgroundTerminal stops ONE running unified-exec
// background PTY on `threadID`, identified by the app-server process id
// the tray row carries as `meta.process_id`. It is the Codex counterpart
// of StopClaudeTask — same tray affordance, different id namespace and a
// different RPC (`thread/backgroundTerminals/terminate`), which is why
// the frontend branches on provider rather than sharing one binding.
//
// The bool is the wire's own answer, not a success flag: `false, nil`
// means the RPC succeeded and matched no running process (the shell had
// already exited, or the id belongs to another thread). Callers must
// surface that as state — a stop that killed nothing emits no follow-up
// `item/completed`, so silently discarding it would leave the user
// staring at a row that never changes.
//
// On a real termination Codex emits `item/completed` for that PTY, which
// flows through the existing triage path and clears the tray row. No
// follow-up work is needed here.
//
// Returns typed errors for:
//
//   - session-missing: no Codex session for this thread. The caller
//     reached the binding before Start / after Close.
//   - provider-mismatch: the thread exists but it's a Claude session.
//     Claude's per-row stop is StopClaudeTask, keyed by task id.
//   - blank process id / timeout / provider error: surfaced verbatim so
//     the UI can render the CLI-supplied message.
func (a *App) TerminateCodexBackgroundTerminal(threadID, processID string) (bool, error) {
	if a.shuttingDown.Load() {
		return false, ErrShuttingDown
	}

	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return false, fmt.Errorf("app: terminate codex background terminal: no active session for thread %s", threadID)
	}
	if sess.codex == nil {
		return false, fmt.Errorf("app: terminate codex background terminal: thread %s is not a Codex thread", threadID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexBackgroundTerminalStopTimeout)
	defer cancel()
	return sess.codex.TerminateBackgroundTerminal(ctx, processID)
}
