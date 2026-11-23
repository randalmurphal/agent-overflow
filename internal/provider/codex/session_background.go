package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// ErrBackgroundTerminalOwnerGone reports that the agent whose thread ran a
// background terminal is gone: this session no longer owns a thread for
// the launch, or Codex no longer has the thread loaded. Codex terminates a
// thread's processes when it shuts the thread down, so nothing is left to
// stop.
var ErrBackgroundTerminalOwnerGone = errors.New("codex: the agent thread that ran the background terminal is gone")

// backgroundTerminalThreads resolves the provider threads whose process
// tables a background-terminal RPC for ownerLaunchID addresses: the root
// thread for "", and otherwise the child threads the spawn launch owns.
// Each Codex thread has its own process table, so a process id means
// nothing outside the thread that started it.
func (s *Session) backgroundTerminalThreads(method, ownerLaunchID string) ([]string, error) {
	ownerLaunchID = strings.TrimSpace(ownerLaunchID)
	if ownerLaunchID == "" {
		rootThreadID := s.rootThreadID()
		if rootThreadID == "" {
			return nil, fmt.Errorf("codex: %s: session has no thread id", method)
		}
		return []string{rootThreadID}, nil
	}
	s.mu.Lock()
	var threads []string
	for childThreadID, launchID := range s.collab.childParentByThread {
		if launchID == ownerLaunchID {
			threads = append(threads, childThreadID)
		}
	}
	s.mu.Unlock()
	if len(threads) == 0 {
		return nil, fmt.Errorf("codex: %s: launch %s owns no thread in this session: %w", method, ownerLaunchID, ErrBackgroundTerminalOwnerGone)
	}
	sort.Strings(threads)
	return threads, nil
}

// backgroundTerminalOwnerGone turns the app-server's refusal to resolve an
// agent's thread into ErrBackgroundTerminalOwnerGone. The background
// terminal methods resolve their thread among the loaded ones and answer
// -32600 "thread not found: <id>" for any other (Codex
// thread_processor.rs load_thread). The root thread stays loaded while the
// session lives, so only an agent scope is read this way.
func backgroundTerminalOwnerGone(err error, ownerLaunchID string) error {
	var rpcErr *RPCError
	if ownerLaunchID != "" && errors.As(err, &rpcErr) && rpcErr.Code == -32600 &&
		strings.HasPrefix(strings.TrimSpace(rpcErr.Message), "thread not found") {
		return errors.Join(ErrBackgroundTerminalOwnerGone, err)
	}
	return err
}

// TerminateBackgroundTerminal kills ONE running background PTY by its
// app-server process id in the thread that runs it (ownerLaunchID: "" for
// the root thread, the spawn launch id for an agent's), and reports whether
// a process was actually terminated. `false, nil` means the RPC succeeded
// but matched nothing: the shell had already exited. That is a state
// answer, not a failure. An agent thread that is gone is
// ErrBackgroundTerminalOwnerGone.
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
func (s *Session) TerminateBackgroundTerminal(ctx context.Context, ownerLaunchID, processID string) (bool, error) {
	const method = "thread/backgroundTerminals/terminate"
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return false, fmt.Errorf("codex: %s: process id required", method)
	}
	threads, err := s.backgroundTerminalThreads(method, ownerLaunchID)
	if err != nil {
		return false, err
	}
	terminated := false
	for _, threadID := range threads {
		killed, err := s.terminateBackgroundTerminalIn(ctx, threadID, processID)
		if err != nil {
			return terminated, backgroundTerminalOwnerGone(err, ownerLaunchID)
		}
		terminated = terminated || killed
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
// running unified-exec background PTY of one thread: the root thread for
// ownerLaunchID "", an agent's thread for its spawn launch id. It is the
// thread-wide sibling of TerminateBackgroundTerminal, the right call for
// "stop everything" (thread delete, revert), not for a per-row stop. An
// agent thread that is gone is ErrBackgroundTerminalOwnerGone.
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
func (s *Session) CleanBackgroundTerminals(ctx context.Context, ownerLaunchID string) error {
	const method = "thread/backgroundTerminals/clean"
	threads, err := s.backgroundTerminalThreads(method, ownerLaunchID)
	if err != nil {
		return err
	}
	for _, threadID := range threads {
		if err := s.cleanBackgroundTerminalsIn(ctx, threadID); err != nil {
			return backgroundTerminalOwnerGone(err, ownerLaunchID)
		}
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
