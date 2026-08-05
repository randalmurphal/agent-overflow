package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// backgroundTerminalListPageLimit bounds the `thread/backgroundTerminals/list`
// pagination walk. A thread's running PTY count is small (one per
// backgrounded `exec_command`); anything past this many pages means the
// server handed us a cursor that never terminates, and we fail loudly
// rather than spin.
const backgroundTerminalListPageLimit = 50

// BackgroundTerminal is one running model-initiated background PTY as
// reported by `thread/backgroundTerminals/list`.
//
// ItemID joins to the transcript row (the `commandExecution` item id AO
// already persists) and ProcessID is the app-server handle
// TerminateBackgroundTerminal takes — the same value the item meta
// carries as `process_id` (see enrichItemMeta). The host-OS metrics are
// nullable upstream (`Option<u32>` / `Option<f64>` / `Option<u64>`), so
// they are pointers here: "not reported" and "zero" are different facts
// and a UI that renders 0% CPU for an unmeasured process is lying.
type BackgroundTerminal struct {
	ItemID     string   `json:"itemId"`
	ProcessID  string   `json:"processId"`
	Command    string   `json:"command"`
	Cwd        string   `json:"cwd"`
	OSPid      *int64   `json:"osPid,omitempty"`
	CPUPercent *float64 `json:"cpuPercent,omitempty"`
	RSSKb      *int64   `json:"rssKb,omitempty"`
}

type backgroundTerminalListResponse struct {
	Data       []BackgroundTerminal `json:"data"`
	NextCursor *string              `json:"nextCursor"`
}

// ListBackgroundTerminals returns every running background PTY for this
// session's thread, following `nextCursor` until the server reports the
// end of the list.
//
// Wire contract (verified on codex-cli 0.146.0, spike-codex capture
// 10-bgterminals-list; type definitions at
// codex-rs/app-server-protocol/src/protocol/v2/thread.rs
// ThreadBackgroundTerminalsList*): request `{threadId, cursor?, limit?}`,
// response `{data: ThreadBackgroundTerminal[], nextCursor: string|null}`.
// The method is `#[experimental]` and needs `capabilities.experimentalApi`,
// which every AO app-server handshake sets.
//
// An empty result is normal and not an error: it means the model has no
// backgrounded shells right now.
func (s *Session) ListBackgroundTerminals(ctx context.Context) ([]BackgroundTerminal, error) {
	rootThreadID := s.rootThreadID()
	if rootThreadID == "" {
		return nil, fmt.Errorf("codex: thread/backgroundTerminals/list: session has no thread id")
	}
	var (
		terminals []BackgroundTerminal
		cursor    string
	)
	for page := 0; page < backgroundTerminalListPageLimit; page++ {
		params := map[string]any{"threadId": rootThreadID}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := s.sendRequest(ctx, "thread/backgroundTerminals/list", params)
		if err != nil {
			return nil, fmt.Errorf("codex: thread/backgroundTerminals/list: %w", err)
		}
		var resp backgroundTerminalListResponse
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &resp); err != nil {
				return nil, fmt.Errorf("codex: thread/backgroundTerminals/list: decode response: %w", err)
			}
		}
		terminals = append(terminals, resp.Data...)
		if resp.NextCursor == nil || strings.TrimSpace(*resp.NextCursor) == "" {
			return terminals, nil
		}
		if *resp.NextCursor == cursor {
			return nil, fmt.Errorf(
				"codex: thread/backgroundTerminals/list: server repeated cursor %q",
				cursor,
			)
		}
		cursor = *resp.NextCursor
	}
	return nil, fmt.Errorf(
		"codex: thread/backgroundTerminals/list: cursor did not terminate after %d pages",
		backgroundTerminalListPageLimit,
	)
}

type backgroundTerminalTerminateResponse struct {
	Terminated bool `json:"terminated"`
}

// TerminateBackgroundTerminal kills ONE running background PTY by its
// app-server process id and reports whether a process was actually
// terminated. `false, nil` means the RPC succeeded but matched nothing —
// the shell had already exited, or the id belongs to another thread.
// That is a state answer, not a failure; callers refresh their list
// rather than surfacing an error.
//
// Wire contract (`thread/backgroundTerminals/terminate`, params
// `{threadId, processId}` → `{terminated: bool}`) is upstream-typed at
// codex-rs/app-server-protocol/src/protocol/v2/thread.rs
// (ThreadBackgroundTerminalsTerminateParams / …TerminateResponse) and
// spike-confirmed on codex-cli 0.146.0: a request that omits processId
// comes back as -32600 "Invalid request: missing field processId"
// (capture 41-bgterminals-terminate-probe).
//
// Version floor: the method shipped in codex 0.140.0, below AO's
// provider-wide floor of provider.minimumCodexCLIVersion (0.143.0), so
// no runtime capability probe is needed — every Codex build AO will talk
// to has it.
// provider.TestMinimumCodexCLIVersionCoversBackgroundTerminalTerminate
// fails if that floor is ever lowered past this method.
func (s *Session) TerminateBackgroundTerminal(ctx context.Context, processID string) (bool, error) {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return false, fmt.Errorf("codex: thread/backgroundTerminals/terminate: process id required")
	}
	// Test-only seam, same shape as cleanBackgroundTerminalsFn. It stands in
	// for the session/wire half only — argument validation above still runs,
	// so a test session cannot accept a process id the real one refuses.
	// Production NewSession never sets it.
	if s.terminateBackgroundTerminalFn != nil {
		return s.terminateBackgroundTerminalFn(ctx, processID)
	}
	rootThreadID := s.rootThreadID()
	if rootThreadID == "" {
		return false, fmt.Errorf("codex: thread/backgroundTerminals/terminate: session has no thread id")
	}
	raw, err := s.sendRequest(ctx, "thread/backgroundTerminals/terminate", map[string]any{
		"threadId":  rootThreadID,
		"processId": processID,
	})
	if err != nil {
		return false, fmt.Errorf("codex: thread/backgroundTerminals/terminate %s: %w", processID, err)
	}
	var resp backgroundTerminalTerminateResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return false, fmt.Errorf(
				"codex: thread/backgroundTerminals/terminate %s: decode response: %w",
				processID, err,
			)
		}
	}
	return resp.Terminated, nil
}

// CleanBackgroundTerminals asks the Codex app-server to terminate every
// running unified-exec background PTY for this session's thread. It is the
// thread-wide sibling of TerminateBackgroundTerminal — the right call for
// "stop everything" (thread delete, tray-level stop-all), not for a
// per-row stop.
//
// Wire contract is owned by the Codex source of truth:
// /home/rmurphy/repos/codex/codex-rs/app-server-protocol/src/protocol/v2/thread.rs
// (ThreadBackgroundTerminalsCleanParams / ThreadBackgroundTerminalsCleanResponse).
// The response body is empty on success — the observable effect is a stream
// of `item/completed` events for each terminated PTY that flow through our
// existing triage path (Phase 2's sibling synthesis fires per completion).
//
// Safe to call from any goroutine: sendRequest handles correlation under
// the session's internal locks. Returns ctx.Err() on cancellation, or a
// wrapped error on RPC failure.
func (s *Session) CleanBackgroundTerminals(ctx context.Context) error {
	// Tests install a cleanBackgroundTerminalsFn override so the binding
	// layer can verify its session-lookup / provider-mismatch plumbing
	// without spinning up a real app-server. Production NewSession never
	// sets it; the wire path below is the only branch that runs in
	// production.
	if s.cleanBackgroundTerminalsFn != nil {
		return s.cleanBackgroundTerminalsFn(ctx)
	}
	// NewSession validates the root thread id during the start/resume
	// handshake and Close's the session on failure, so a live Session always
	// has this populated. The explicit guard mirrors Probe/Resume so a future
	// caller that constructs a partial Session for testing still gets a
	// specific error rather than the app-server rejecting an empty threadId
	// with a less actionable message.
	rootThreadID := s.rootThreadID()
	if rootThreadID == "" {
		return fmt.Errorf("codex: thread/backgroundTerminals/clean: session has no thread id")
	}
	if _, err := s.sendRequest(ctx, "thread/backgroundTerminals/clean", map[string]any{
		"threadId": rootThreadID,
	}); err != nil {
		return fmt.Errorf("codex: thread/backgroundTerminals/clean: %w", err)
	}
	return nil
}
