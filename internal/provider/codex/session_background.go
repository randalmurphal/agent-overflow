package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

// ListBackgroundTerminals returns every running background PTY of one
// Codex thread (scopeID, a command's ProcessScope.ID: the root thread or an
// agent's), following `nextCursor` until the server reports the end of the
// list. A thread Codex no longer has loaded is
// ErrBackgroundTerminalThreadNotLoaded.
//
// Wire contract (verified on codex-cli 0.146.0, spike-codex capture
// 10-bgterminals-list; type definitions at
// codex-rs/app-server-protocol/src/protocol/v2/thread.rs
// ThreadBackgroundTerminalsList*): request `{threadId, cursor?, limit?}`,
// response `{data: ThreadBackgroundTerminal[], nextCursor: string|null}`.
// The method is `#[experimental]` and needs `capabilities.experimentalApi`,
// which every AO app-server handshake sets.
//
// An empty result is normal and not an error: it means the thread has no
// backgrounded shells right now.
func (s *Session) ListBackgroundTerminals(ctx context.Context, scopeID string) ([]BackgroundTerminal, error) {
	const method = "thread/backgroundTerminals/list"
	threadID, err := s.backgroundTerminalThread(method, scopeID)
	if err != nil {
		return nil, err
	}
	var (
		terminals []BackgroundTerminal
		cursor    string
	)
	for page := 0; page < backgroundTerminalListPageLimit; page++ {
		raw, err := s.listBackgroundTerminalsPage(ctx, threadID, cursor)
		if err != nil {
			return nil, s.backgroundTerminalThreadGone(threadID, fmt.Errorf("codex: %s: %w", method, err))
		}
		var resp backgroundTerminalListResponse
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &resp); err != nil {
				return nil, fmt.Errorf("codex: %s: decode response: %w", method, err)
			}
		}
		terminals = append(terminals, resp.Data...)
		if resp.NextCursor == nil || strings.TrimSpace(*resp.NextCursor) == "" {
			return terminals, nil
		}
		if *resp.NextCursor == cursor {
			return nil, fmt.Errorf("codex: %s: server repeated cursor %q", method, cursor)
		}
		cursor = *resp.NextCursor
	}
	return nil, fmt.Errorf("codex: %s: cursor did not terminate after %d pages", method, backgroundTerminalListPageLimit)
}

func (s *Session) listBackgroundTerminalsPage(ctx context.Context, threadID, cursor string) (json.RawMessage, error) {
	// Test-only seam for the wire read; scope validation above still runs.
	// Production NewSession never sets it.
	if s.listBackgroundTerminalsFn != nil {
		return s.listBackgroundTerminalsFn(ctx, threadID, cursor)
	}
	params := map[string]any{"threadId": threadID}
	if cursor != "" {
		params["cursor"] = cursor
	}
	return s.sendRequest(ctx, "thread/backgroundTerminals/list", params)
}

type backgroundTerminalTerminateResponse struct {
	Terminated bool `json:"terminated"`
}

// ErrBackgroundTerminalThreadNotLoaded reports that Codex no longer has the
// agent thread a background-terminal call named loaded. Codex terminates a
// thread's processes when it shuts the thread down and drops the thread only
// after that (core agent/control legacy.rs shutdown_live_agent), so none of
// its processes is left.
var ErrBackgroundTerminalThreadNotLoaded = errors.New("codex: the agent thread is no longer loaded")

// backgroundTerminalThread validates the thread a background-terminal call
// names: scopeID is a command's ProcessScope.ID. A review run's scope is
// not addressable (reviewProcessScope), so a caller that sends one has
// skipped ProcessScope.Addressable.
func (s *Session) backgroundTerminalThread(method, scopeID string) (string, error) {
	scopeID = strings.TrimSpace(scopeID)
	if scopeID == "" {
		return "", fmt.Errorf("codex: %s: process scope required", method)
	}
	if isReviewProcessScope(scopeID) {
		return "", fmt.Errorf("codex: %s: %s runs inside a code review, which no background-terminal call reaches", method, scopeID)
	}
	return scopeID, nil
}

// backgroundTerminalThreadGone reads the app-server's refusal to resolve a
// thread. The background terminal methods resolve their thread among the
// loaded ones and answer -32600 "thread not found: <id>" for any other
// (Codex thread_processor.rs load_thread). The root thread stays loaded
// while the session lives, so only an agent's thread is read this way: it
// is the thread's unload, reported in the thread's frame order
// (observeChildThreadGone).
func (s *Session) backgroundTerminalThreadGone(threadID string, err error) error {
	var rpcErr *RPCError
	if threadID == s.rootThreadID() || !errors.As(err, &rpcErr) || rpcErr.Code != -32600 ||
		!strings.HasPrefix(strings.TrimSpace(rpcErr.Message), "thread not found") {
		return err
	}
	s.observeChildThreadGone(threadID)
	return errors.Join(ErrBackgroundTerminalThreadNotLoaded, err)
}

// observeChildThreadGone handles Codex's answer that it has no agent thread
// providerThreadID loaded as the thread's own notLoaded status: the same
// fact, through the same path, in the same order. The read loop has handed
// every frame it read before the answer to the session by the time the
// caller has it, so the status is dispatched after the thread's command
// starts, behind any of them still queued. A thread with no route has
// nothing to end: its route retires only after its unload was reported.
func (s *Session) observeChildThreadGone(providerThreadID string) {
	const method = "thread/status/changed"
	if s.parentToolUseForProviderThread(providerThreadID) == "" {
		return
	}
	params, err := json.Marshal(map[string]any{
		"threadId": providerThreadID,
		"status":   map[string]string{"type": "notLoaded"},
	})
	if err != nil {
		log.Printf("codex: agent thread %s: encode its unload: %v", providerThreadID, err)
		return
	}
	s.dispatchNotification(method, params)
}

// TerminateBackgroundTerminal asks Codex to kill ONE running background PTY
// by its app-server process id in the thread that runs it (scopeID, the
// command's ProcessScope.ID). `false, nil` is not proof the process is
// gone: Codex answers false both when the thread has no such process and
// when it failed to terminate one (core unified_exec process_manager.rs
// terminate_process). The command's completion or its thread's unload says
// when it ended. A thread Codex no longer has loaded is
// ErrBackgroundTerminalThreadNotLoaded.
//
// Wire contract (`thread/backgroundTerminals/terminate`, params
// `{threadId, processId}` → `{terminated: bool}`) is upstream-typed at
// codex-rs/app-server-protocol/src/protocol/v2/thread.rs
// (ThreadBackgroundTerminalsTerminateParams / …TerminateResponse) and
// spike-confirmed on codex-cli 0.146.0: a request that omits processId
// comes back as -32600 "Invalid request: missing field processId"
// (capture 41-bgterminals-terminate-probe). The app-server resolves
// threadId among every loaded thread, an agent's included.
//
// Version floor: the method shipped in codex 0.140.0, below AO's
// provider-wide floor of provider.minimumCodexCLIVersion (0.143.0), so
// no runtime capability probe is needed — every Codex build AO will talk
// to has it.
// provider.TestMinimumCodexCLIVersionCoversBackgroundTerminalTerminate
// fails if that floor is ever lowered past this method.
func (s *Session) TerminateBackgroundTerminal(ctx context.Context, scopeID, processID string) (bool, error) {
	const method = "thread/backgroundTerminals/terminate"
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return false, fmt.Errorf("codex: %s: process id required", method)
	}
	threadID, err := s.backgroundTerminalThread(method, scopeID)
	if err != nil {
		return false, err
	}
	terminated, err := s.terminateBackgroundTerminalIn(ctx, threadID, processID)
	if err != nil {
		return false, s.backgroundTerminalThreadGone(threadID, err)
	}
	return terminated, nil
}

func (s *Session) terminateBackgroundTerminalIn(ctx context.Context, threadID, processID string) (bool, error) {
	// Test-only seam for the wire write; ownership resolution and argument
	// validation above still run. Production NewSession never sets it.
	if s.terminateBackgroundTerminalFn != nil {
		return s.terminateBackgroundTerminalFn(ctx, threadID, processID)
	}
	raw, err := s.sendRequest(ctx, "thread/backgroundTerminals/terminate", map[string]any{
		"threadId":  threadID,
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
// running unified-exec background PTY of one thread (scopeID, a command's
// ProcessScope.ID). It is the thread-wide sibling of
// TerminateBackgroundTerminal, the right call for "stop everything" (thread
// delete, revert), not for a per-row stop. Its answer only acknowledges that
// Codex took the request (thread_processor.rs submits
// Op::CleanBackgroundTerminals and returns). A thread Codex no longer has
// loaded is ErrBackgroundTerminalThreadNotLoaded.
//
// Wire contract is owned by the Codex source of truth:
// https://github.com/openai/codex/blob/main/codex-rs/app-server-protocol/src/protocol/v2/thread.rs
// (ThreadBackgroundTerminalsCleanParams / ThreadBackgroundTerminalsCleanResponse).
// The response body is empty on success — the observable effect is a stream
// of `item/completed` events for each terminated PTY that flow through our
// existing triage path.
//
// Safe to call from any goroutine: sendRequest handles correlation under
// the session's internal locks. Returns ctx.Err() on cancellation, or a
// wrapped error on RPC failure.
func (s *Session) CleanBackgroundTerminals(ctx context.Context, scopeID string) error {
	threadID, err := s.backgroundTerminalThread("thread/backgroundTerminals/clean", scopeID)
	if err != nil {
		return err
	}
	if err := s.cleanBackgroundTerminalsIn(ctx, threadID); err != nil {
		return s.backgroundTerminalThreadGone(threadID, err)
	}
	return nil
}

func (s *Session) cleanBackgroundTerminalsIn(ctx context.Context, threadID string) error {
	// Test-only seam for the wire write; ownership resolution above still
	// runs. Production NewSession never sets it.
	if s.cleanBackgroundTerminalsFn != nil {
		return s.cleanBackgroundTerminalsFn(ctx, threadID)
	}
	if _, err := s.sendRequest(ctx, "thread/backgroundTerminals/clean", map[string]any{
		"threadId": threadID,
	}); err != nil {
		return fmt.Errorf("codex: thread/backgroundTerminals/clean: %w", err)
	}
	return nil
}
