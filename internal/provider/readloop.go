package provider

import (
	"encoding/json"
	"time"
)

// EmitTeardownStatus emits the two session-status events every provider read
// loop ends with, in the one order they are allowed to arrive in: the
// abnormal-exit "error" carrying the process-exit meta (only when the host
// did NOT initiate the close), then "disconnected", unconditionally.
//
// Why this is not optional, and why it lives in one place: triage gates
// SYNTHESIZING the truncated turn-complete on the "error" signal. A read loop
// that exits without emitting it leaves the frontend's working indicator
// spinning on a thread whose process is already gone — with no later event
// that can clear it, because the process that would have sent one is dead.
// "Abnormal" here includes a clean exit-code-0 that nobody asked for: the
// exit code says how the process died, `closing` says whether AO meant it,
// and only the second question decides whether a turn was cut off.
//
// closing is the caller's own closing flag, read at teardown. proc may be nil
// (a fixture-built session that never spawned); the exit meta then reports
// what a nil exit error reports, which is what MarshalProcessExitMeta already
// tolerates for a clean or un-reaped exit.
//
// Provider-specific drains — Claude's pending control requests, Codex's
// JSON-RPC waiters, both approval registries — stay in their own read loops
// and run BEFORE this: they release callers parked on a reply, and a caller
// still parked when the "disconnected" lands would outlive the session.
func EmitTeardownStatus(onEvent func(ProviderEvent), threadID string, proc *Process, closing bool) {
	var exitMeta json.RawMessage
	if !closing {
		// WaitProcessExitErr can return nil for a clean exit or for a reap
		// timeout; MarshalProcessExitMeta handles both.
		var exitErr error
		var stderrTail string
		if proc != nil {
			exitErr = WaitProcessExitErr(proc)
			stderrTail = proc.StderrTail()
		}
		exitMeta = MarshalProcessExitMeta(exitErr, stderrTail)
	}
	EmitTeardownStatusWithMeta(onEvent, threadID, exitMeta, closing)
}

// EmitTeardownStatusWithMeta is EmitTeardownStatus for a transport whose
// death is not a *Process exit — claudetui, whose session dies with a PTY and
// whose meta is a terminal exit status. Same two events, same order, same
// reason; only the source of the abnormal-exit meta differs.
//
// exitMeta is ignored when closing is true, because no "error" is emitted at
// all in that case.
func EmitTeardownStatusWithMeta(onEvent func(ProviderEvent), threadID string, exitMeta json.RawMessage, closing bool) {
	if onEvent == nil {
		return
	}

	if !closing {
		onEvent(ProviderEvent{
			Kind:      EventSessionStatus,
			ThreadID:  threadID,
			Content:   "error",
			Meta:      exitMeta,
			Timestamp: time.Now(),
		})
	}

	onEvent(ProviderEvent{
		Kind:      EventSessionStatus,
		ThreadID:  threadID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})
}
