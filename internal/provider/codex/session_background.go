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
		raw, err := s.sendTerminalRequest(ctx, threadID, s.listPageCall(threadID, cursor, 0))
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

// listPageCall is one `thread/backgroundTerminals/list` page of threadID
// from cursor, at most limit rows when limit is above zero.
func (s *Session) listPageCall(threadID, cursor string, limit int) terminalCall {
	params := map[string]any{"threadId": threadID}
	if cursor != "" {
		params["cursor"] = cursor
	}
	if limit > 0 {
		params["limit"] = limit
	}
	call := terminalCall{method: "thread/backgroundTerminals/list", params: params}
	// Test-only seam for the wire read; admission and the reading of the
	// answer still run. Production NewSession never sets it.
	if s.listBackgroundTerminalsFn != nil {
		call.wire = func(ctx context.Context) (json.RawMessage, error) {
			return s.listBackgroundTerminalsFn(ctx, threadID, cursor)
		}
	}
	return call
}

// terminalCall is one background-terminal request: its method and params,
// and the test wire that answers it in place of the app-server (nil in
// production).
type terminalCall struct {
	method string
	params map[string]any
	wire   func(ctx context.Context) (json.RawMessage, error)
}

// sendTerminalRequest sends call to threadID, if the session may (see
// admitTerminalRequest), and waits for the answer.
func (s *Session) sendTerminalRequest(ctx context.Context, threadID string, call terminalCall) (json.RawMessage, error) {
	release, err := s.admitTerminalRequest(call.method, threadID)
	if err != nil {
		return nil, err
	}
	wait, err := s.startTerminalRequest(threadID, call)
	release()
	if err != nil {
		return nil, err
	}
	return wait(ctx)
}

// admitTerminalRequest admits a background-terminal request to threadID:
// the session's own thread or an agent thread it owns now, and only before
// Close began. The caller writes the request and then calls release. Close
// marks the session closed under terminalWriteMu's write lock, so a request
// admitted before has been written by then and none is written after: a
// session that ended cannot reach a thread that another session holds now.
func (s *Session) admitTerminalRequest(method, threadID string) (release func(), err error) {
	s.terminalWriteMu.RLock()
	if s.terminalsClosed {
		s.terminalWriteMu.RUnlock()
		return nil, fmt.Errorf("codex: %s: the session is ending", method)
	}
	if !s.runsThread(threadID) {
		s.terminalWriteMu.RUnlock()
		return nil, fmt.Errorf("codex: %s: thread %s is neither this session's own thread nor an agent's it runs", method, threadID)
	}
	return s.terminalWriteMu.RUnlock, nil
}

// closeTerminalRequests is Close's first step for the background-terminal
// requests: once it returns, every admitted request has been written and no
// other will be (admitTerminalRequest).
func (s *Session) closeTerminalRequests() {
	s.terminalWriteMu.Lock()
	s.terminalsClosed = true
	s.terminalWriteMu.Unlock()
}

// runsThread reports whether threadID is the session's root thread or an
// agent thread it owns now.
func (s *Session) runsThread(threadID string) bool {
	if threadID == "" {
		return false
	}
	if threadID == s.rootThreadID() {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.collab.childParentByThread[threadID] != ""
}

// startTerminalRequest writes call to threadID and returns the wait for its
// answer. Codex's answer that it has no such thread loaded is that agent
// thread's unload, and the read loop dispatches it at the answer's place in
// the stream, before it reads the next frame (noteTerminalAnswer): a frame
// Codex wrote after the answer, such as the same thread loaded again and
// starting a command, comes after the unload. A test wire's answer is read
// when the wire answers.
func (s *Session) startTerminalRequest(threadID string, call terminalCall) (func(context.Context) (json.RawMessage, error), error) {
	if call.wire != nil {
		return func(ctx context.Context) (json.RawMessage, error) {
			raw, err := call.wire(ctx)
			s.noteTerminalAnswer(threadID, err)
			return raw, err
		}, nil
	}
	return s.startRequestObserved(call.method, call.params, func(line json.RawMessage) {
		_, err := decodeRPCReply(call.method, line)
		s.noteTerminalAnswer(threadID, err)
	})
}

// noteTerminalAnswer reads a background-terminal answer from threadID: a
// refusal that Codex has no such thread loaded is an agent thread's unload
// (observeChildThreadGone).
func (s *Session) noteTerminalAnswer(threadID string, err error) {
	if threadID != s.rootThreadID() && isThreadNotLoadedAnswer(err) {
		s.observeChildThreadGone(threadID)
	}
}

// isThreadNotLoadedAnswer reports Codex's refusal to resolve a thread it
// has not loaded: the background terminal methods resolve their thread among
// the loaded ones and answer -32600 "thread not found: <id>" for any other
// (Codex thread_processor.rs load_thread).
func isThreadNotLoadedAnswer(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == -32600 &&
		strings.HasPrefix(strings.TrimSpace(rpcErr.Message), "thread not found")
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

// backgroundTerminalThreadGone is the caller's error for Codex's refusal to
// resolve an agent thread (isThreadNotLoadedAnswer):
// ErrBackgroundTerminalThreadNotLoaded, the thread's unload having been
// dispatched where the answer was read (startTerminalRequest). The root
// thread stays loaded while the session lives, so its refusal is a plain
// error.
func (s *Session) backgroundTerminalThreadGone(threadID string, err error) error {
	if threadID == s.rootThreadID() || !isThreadNotLoadedAnswer(err) {
		return err
	}
	return errors.Join(ErrBackgroundTerminalThreadNotLoaded, err)
}

// observeChildThreadGone dispatches an agent thread's unload that Codex said
// other than by the thread's notLoaded status, a "thread not found" answer
// or a thread/closed, as that status: the same fact, through the same path.
// It is called where the fact was read, before the next frame is, so the
// status comes after every frame of the thread read before it, behind any
// still queued, and before any read after it. A thread with no route has
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
	call := terminalCall{method: method, params: map[string]any{"threadId": threadID, "processId": processID}}
	// Test-only seam for the wire write; admission, argument validation and
	// the reading of the answer still run. Production NewSession never sets it.
	if s.terminateBackgroundTerminalFn != nil {
		call.wire = func(ctx context.Context) (json.RawMessage, error) {
			terminated, err := s.terminateBackgroundTerminalFn(ctx, threadID, processID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(backgroundTerminalTerminateResponse{Terminated: terminated})
		}
	}
	raw, err := s.sendTerminalRequest(ctx, threadID, call)
	if err != nil {
		return false, s.backgroundTerminalThreadGone(threadID, fmt.Errorf("codex: %s %s: %w", method, processID, err))
	}
	var resp backgroundTerminalTerminateResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return false, fmt.Errorf("codex: %s %s: decode response: %w", method, processID, err)
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
	_, err = s.sendTerminalRequest(ctx, threadID, s.cleanCall(threadID))
	return s.cleanAnswer(threadID, err)
}

// cleanCall is a `thread/backgroundTerminals/clean` of threadID.
func (s *Session) cleanCall(threadID string) terminalCall {
	call := terminalCall{method: "thread/backgroundTerminals/clean", params: map[string]any{"threadId": threadID}}
	// Test-only seam for the wire write; admission and the reading of the
	// answer still run. Production NewSession never sets it.
	if s.cleanBackgroundTerminalsFn != nil {
		call.wire = func(ctx context.Context) (json.RawMessage, error) {
			return nil, s.cleanBackgroundTerminalsFn(ctx, threadID)
		}
	}
	return call
}

// cleanAnswer is a clean's error for its caller.
func (s *Session) cleanAnswer(threadID string, err error) error {
	if err == nil {
		return nil
	}
	return s.backgroundTerminalThreadGone(threadID, fmt.Errorf("codex: thread/backgroundTerminals/clean: %w", err))
}

// cleanForClose is Close's clean of threadID's terminals: Close has stopped
// admitting requests (closeTerminalRequests), so it writes its own.
func (s *Session) cleanForClose(ctx context.Context, threadID string) error {
	wait, err := s.startTerminalRequest(threadID, s.cleanCall(threadID))
	if err == nil {
		_, err = wait(ctx)
	}
	return s.cleanAnswer(threadID, err)
}

// probeChildThread asks Codex whether it still has an agent thread loaded,
// with the smallest request that answers it: one list page of at most one
// row. Its "thread not found" is the thread's unload (startTerminalRequest);
// any other error is returned.
func (s *Session) probeChildThread(ctx context.Context, threadID string) error {
	_, err := s.sendTerminalRequest(ctx, threadID, s.listPageCall(threadID, "", 1))
	if err != nil {
		return s.backgroundTerminalThreadGone(threadID, fmt.Errorf("codex: thread/backgroundTerminals/list: %w", err))
	}
	return nil
}
