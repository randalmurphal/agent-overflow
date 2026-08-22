package main

import (
	"context"
	"fmt"
	"time"
)

// stopClaudeTaskTimeout bounds the Wails-side wait on StopTask and its
// sibling BackgroundTask. We pass this context into the Claude session's
// round-trip so the binding can never hang — if the subprocess wedges,
// the caller sees a timeout error in at most this window.
//
// The spike saw sub-100ms round-trips on Claude CLI 2.1.112. Ten
// seconds is a generous ceiling that still fails loudly; the per-
// session control-request timeout is the same
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
//     not a Claude one. Codex's per-row stop is a different RPC with a
//     different id namespace (process id, not task id) — see
//     codex.Session.TerminateBackgroundTerminal and
//     docs/references/codex.md#background-terminals; the frontend must
//     branch on provider before reaching for this.
//   - timeout / provider error: surfaced verbatim so the UI can render
//     the CLI-supplied message.
func (a *App) StopClaudeTask(threadID, taskID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}

	sess, ok := a.sessionManager().get(threadID)
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

// BackgroundClaudeTask moves an in-flight FOREGROUND Claude task (a
// running subagent or a foreground Bash) to the background, identified
// by the `toolUseID` of the block that started it — the control-request
// form of the Claude TUI's Ctrl+B, and the wire behind the background
// button on a running agent / Bash row.
//
// Keyed by tool_use_id, NOT task_id, because that is what the CLI's
// `background_tasks` subtype takes and because the UI's button lives on
// the launch row, whose id IS the tool_use_id. StopClaudeTask is the
// sibling in the opposite direction and takes a task_id — the two ids
// are not interchangeable, which is why the bindings do not share one
// parameter name.
//
// On success the CLI answers `{backgrounded:true}` and then emits
// `system/task_updated {patch:{is_backgrounded:true}}`, which flows
// through triage as EventSubagentBackgrounded and stamps the launch row
// with the moment its sidechain streaming stopped.
//
// Returns typed errors for:
//
//   - session-missing: no Claude session for this thread. The caller
//     backgrounded before Start / after Close.
//   - provider-mismatch: the thread exists but it's a Codex session.
//     Codex has no equivalent — a spawned collab-agent child is already
//     asynchronous and `close_agent` is a model-only tool — so the
//     frontend must branch on provider before reaching for this.
//   - timeout / provider error: surfaced verbatim, including the CLI's
//     refusal when no foreground task matched the tool_use_id.
func (a *App) BackgroundClaudeTask(threadID, toolUseID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}

	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return fmt.Errorf("app: background claude task: no active session for thread %s", threadID)
	}
	if sess.claude == nil {
		return fmt.Errorf("app: background claude task: thread %s is not a Claude thread", threadID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), stopClaudeTaskTimeout)
	defer cancel()
	return sess.claude.BackgroundTask(ctx, toolUseID)
}
