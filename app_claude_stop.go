package main

import (
	"context"
	"fmt"
	"time"
)

// stopClaudeTaskTimeout bounds the Wails-side wait on StopTask. We pass
// this context into the Claude session's round-trip so the binding can
// never hang — if the subprocess wedges, the caller sees a timeout
// error in at most this window.
//
// The spike saw sub-100ms round-trips on Claude CLI 2.1.112. Ten
// seconds is a generous ceiling that still fails loudly; the per-
// session stop_task timeout (DefaultStopTaskTimeout) is the same
// value, so the binding only adds a second layer against a wedged
// session.mu acquisition in the claude package itself.
const stopClaudeTaskTimeout = 10 * time.Second

// StopClaudeTask asks the Claude CLI to kill a backgrounded task
// (run_in_background Bash or a Task subagent) identified by `taskID`.
// On success the CLI emits a follow-up `system/task_updated` with
// `patch.status:"killed"` on the normal event stream, which flows
// through triage into the sibling `tool_completion` row as
// status=killed — rendered as a distinct "Stopped" badge in the UI.
//
// Returns typed errors for:
//
//   - session-missing: no Claude session for this thread. The caller
//     started a stop before Start / after Close.
//   - provider-mismatch: the thread exists but it's a Codex session,
//     not a Claude one. Codex has no per-row stop primitive
//     (see docs/references/codex.md#known-upstream-constraints); the
//     frontend must branch on provider before reaching for this.
//   - timeout / provider error: surfaced verbatim so the UI can render
//     the CLI-supplied message.
func (a *App) StopClaudeTask(threadID, taskID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}

	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("app: stop claude task: no active session for thread %s", threadID)
	}
	if sess.claude == nil {
		return fmt.Errorf("app: stop claude task: thread %s is not a Claude thread", threadID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), stopClaudeTaskTimeout)
	defer cancel()
	return sess.claude.StopTask(ctx, taskID)
}
