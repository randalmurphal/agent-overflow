package main

import (
	"context"
	"fmt"
	"time"
)

// cleanCodexBackgroundTerminalsTimeout bounds the Wails-side wait on the
// thread-wide clean RPC. The app-server normally terminates background
// PTYs in a single tick; the observable effect is a follow-up stream of
// item/completed events, not a slow response. 10s is a generous ceiling
// that still fails loudly if the subprocess wedges — mirrors the Claude
// stop-task binding's budget for the same reason.
const cleanCodexBackgroundTerminalsTimeout = 10 * time.Second

// CleanCodexBackgroundTerminals asks the Codex app-server to terminate
// every running unified-exec background PTY for `threadID`. This is the
// thread-wide "Stop all" primitive for Codex — the protocol exposes no
// per-process kill RPC for model-initiated backgrounded commands
// (see docs/references/codex.md#known-upstream-constraints). The
// frontend Stop-all button lands in Phase 5; this binding is the
// plumbing it will call.
//
// After the RPC succeeds, Codex emits one `item/completed` notification
// per terminated PTY. Those flow through our existing triage path —
// Phase 2's sibling-synthesis stamps the `tool_completion` row and the
// tray reconciles on its own. No follow-up work is needed here.
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

	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("app: clean codex background terminals: no active session for thread %s", threadID)
	}
	if sess.codex == nil {
		return fmt.Errorf("app: clean codex background terminals: thread %s is not a Codex thread", threadID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cleanCodexBackgroundTerminalsTimeout)
	defer cancel()
	return sess.codex.CleanBackgroundTerminals(ctx)
}
