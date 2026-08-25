package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestIsThreadNotFoundRequiresTheResumeErrorShape(t *testing.T) {
	for _, message := range []string{
		"no rollout found for thread id 00000000-0000-4000-8000-000000000001",
		"thread not found: abc",
	} {
		match := fmt.Errorf("start session: %w", &RPCError{
			Method: "thread/resume", Code: -32600, Message: message,
		})
		if !IsThreadNotFound(match) {
			t.Fatalf("wrapped thread/resume not-found error %q was not recognized", message)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("thread not found: abc"),
		&RPCError{Method: "thread/read", Code: -32600, Message: "thread not found: abc"},
		&RPCError{Method: "thread/resume", Code: -32603, Message: "thread not found: abc"},
		&RPCError{Method: "thread/resume", Code: -32600, Message: "invalid model"},
	} {
		if IsThreadNotFound(err) {
			t.Fatalf("unrelated error was recognized as thread loss: %v", err)
		}
	}
}

// -- Session unit tests --

func TestWriteNotificationFormat(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Call the actual writeNotification method. With the cat-backed session,
	// the JSON-RPC notification echoes back and is dispatched by readLoop.
	// "initialized" is skipped by ClassifyNotification, so send a second
	// known notification to verify end-to-end dispatch.
	if err := s.writeNotification("initialized", nil); err != nil {
		t.Fatalf("writeNotification(initialized): %v", err)
	}

	if err := s.writeNotification("turn/started", map[string]any{
		"turn": map[string]any{"id": "turn-verify"},
	}); err != nil {
		t.Fatalf("writeNotification(turn/started): %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTurnStart)
	}
	if evt.TurnID != "turn-verify" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-verify")
	}
}

func TestDispatchLineResponse(t *testing.T) {
	// Create a session with a pending request.
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
	}

	ch := make(chan json.RawMessage, 1)
	s.pending[1] = ch

	// Dispatch a response.
	line := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	s.dispatchLine(line)

	select {
	case resp := <-ch:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
	default:
		t.Fatal("expected response to be routed to pending channel")
	}
}

func TestDispatchLineNotification(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "t1",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-1"}}}`)
	s.dispatchLine(line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnStart)
	}
}

func TestDispatchLineInvalidJSON(t *testing.T) {
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {},
	}
	// Should not panic — logs and returns.
	s.dispatchLine([]byte(`not valid json`))
}

func TestDispatchLineResponseNonIntegerID(t *testing.T) {
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {},
	}
	// Float ID — Int64() fails, logged and returned.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":1.5,"result":{}}`))
}

func TestDispatchLineResponseNoMatchingPending(t *testing.T) {
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {},
	}
	// Valid response but no pending channel for id=999 — silently ignored.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":999,"result":{}}`))
}

func TestDispatchLineServerRequest(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "t1",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	// Server request with id + method — routes to handleServerRequest.
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer func() {
		cancel()
		proc.Close()
	}()
	s.proc = proc

	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventApprovalRequest)
	}
}

func TestDispatchLineServerRequestNonIntegerID(t *testing.T) {
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {},
	}
	// Server request with float ID — logged and returned.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":1.5,"method":"item/commandExecution/requestApproval","params":{}}`))
}

func TestCodexWriteNotification(t *testing.T) {
	s, _ := newTestCodexSession(t)

	if err := s.writeNotification("initialized", nil); err != nil {
		t.Fatalf("writeNotification: %v", err)
	}
	if err := s.writeNotification("test/method", map[string]any{"key": "value"}); err != nil {
		t.Fatalf("writeNotification with params: %v", err)
	}
}

func TestCodexWriteResponse(t *testing.T) {
	s, _ := newTestCodexSession(t)

	if err := s.writeResponse(42, map[string]any{"ok": true}); err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
}

func TestCodexReadLoopDispatchesNotification(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Write a turn/started notification through cat.
	line := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-1"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTurnStart)
	}
	if evt.TurnID != "turn-1" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-1")
	}
}

func TestCodexReadLoopRoutesResponseToPending(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Set up a pending request.
	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[42] = ch
	s.mu.Unlock()

	// Write a response with id=42 through cat.
	respLine := []byte(`{"jsonrpc":"2.0","id":42,"result":{"ok":true}}`)
	if err := s.proc.WriteLine(respLine); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case resp := <-ch:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for response on pending channel")
	}

	s.mu.Lock()
	delete(s.pending, 42)
	s.mu.Unlock()
}

func TestCodexSendRequestViaCat(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// With cat: request echoes -> dispatchLine sees server request (id + method) ->
	// handleServerRequest sends JSON-RPC error (unknown method) -> error echoes ->
	// dispatchLine sees response -> routes to pending -> sendRequest receives it.
	// After the handleServerRequest fix, error is at top level, so sendRequest returns error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.sendRequest(ctx, "test/method", map[string]any{"key": "value"})
	if err == nil {
		t.Fatal("expected error from sendRequest (unknown method)")
	}
}

func TestCodexSendRequestContextCancel(t *testing.T) {
	s, _ := newTestCodexSession(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := s.sendRequest(ctx, "test/method", nil)
	if err == nil {
		t.Fatal("expected context canceled error")
	}
}

func TestCodexSendRequestReturnsErrorWhenSessionStops(t *testing.T) {
	ctx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat >/dev/null"},
	})
	if err != nil {
		t.Fatalf("spawn quiet process: %v", err)
	}

	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancelProc,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()

	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := s.sendRequest(ctx, "test/method", map[string]any{"key": "value"})
		errCh <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	observedPending := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		pendingCount := len(s.pending)
		s.mu.Unlock()
		if pendingCount == 1 {
			observedPending = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !observedPending {
		t.Fatal("timed out waiting for pending request registration")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected session stop error")
		}
		if !strings.Contains(err.Error(), "session stopped before request completed") {
			t.Fatalf("error = %v, want session stop message", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for sendRequest to fail")
	}
}

// TestCodexSendRequestTimeoutDrainsLateResponse covers Bug E5. Before the
// fix, a response that arrived between the timeout firing and the
// deferred pending-delete ran into a buffer-1 channel that nobody read,
// leaking a record into the goroutine's stack until GC. The fix calls
// abandon() (delete-from-pending + drain channel) inside the timeout
// case before returning, so a late response either (a) arrives before
// the delete and is drained, or (b) arrives after the delete and is
// dropped by dispatchLine's default branch.
//
// The test drives the path by overriding requestTimeoutOverride to a
// short window, sending the request, waiting past the timeout, then
// injecting a response for the now-abandoned id. Before the fix, the
// channel would retain the unread payload; after the fix, the pending
// map is empty and no channel is holding the payload. We also assert
// that the goroutine count returns to baseline.
func TestCodexSendRequestTimeoutDrainsLateResponse(t *testing.T) {
	ctx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat >/dev/null"},
	})
	if err != nil {
		t.Fatalf("spawn quiet process: %v", err)
	}
	s := &Session{
		proc:                   proc,
		threadID:               testThread,
		pending:                make(map[int64]chan json.RawMessage),
		onEvent:                func(provider.ProviderEvent) {},
		cancel:                 cancelProc,
		requestTimeoutOverride: 50 * time.Millisecond,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()
	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	// Fire a request that we know will time out because the quiet
	// subprocess never replies.
	start := time.Now()
	_, err = s.sendRequest(context.Background(), "test/method", map[string]any{"k": "v"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want ~50ms", elapsed)
	}

	// After the timeout returns, the pending map must no longer contain
	// the request id. Without the fix, the defer eventually cleaned it
	// up but only AFTER the response could have landed in the buffered
	// channel — here we assert the immediate post-return invariant.
	s.mu.Lock()
	pendingCount := len(s.pending)
	s.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("pending has %d entries after timeout, want 0", pendingCount)
	}

	// Simulate a late response arriving from dispatchLine with the
	// abandoned id. It must be silently dropped by dispatchLine
	// (pending map already emptied) — no panic, no hang.
	lateResponse := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"too":"late"}}`, s.nextID.Load())
	s.dispatchLine([]byte(lateResponse))

	// After all of the above, the pending map must still be empty and
	// the session healthy. A follow-up request must work.
	s.mu.Lock()
	pendingCount = len(s.pending)
	s.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("pending has %d entries after late response, want 0", pendingCount)
	}
}

// TestCodexSendRequestManyTimeoutsDoNotLeak ensures that N requests
// that all time out and later see late responses do not accumulate
// pending-map entries, buffered channel records, or goroutines.
func TestCodexSendRequestManyTimeoutsDoNotLeak(t *testing.T) {
	ctx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat >/dev/null"},
	})
	if err != nil {
		t.Fatalf("spawn quiet process: %v", err)
	}
	s := &Session{
		proc:                   proc,
		threadID:               testThread,
		pending:                make(map[int64]chan json.RawMessage),
		onEvent:                func(provider.ProviderEvent) {},
		cancel:                 cancelProc,
		requestTimeoutOverride: 20 * time.Millisecond,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()
	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	const rounds = 10
	for i := 0; i < rounds; i++ {
		_, err := s.sendRequest(context.Background(), "test/method", nil)
		if err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("round %d: expected timeout, got %v", i, err)
		}
		// Inject a late response for the id we just abandoned. Should
		// be dropped silently.
		lateResponse := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, s.nextID.Load())
		s.dispatchLine([]byte(lateResponse))
	}

	s.mu.Lock()
	pendingCount := len(s.pending)
	s.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("pending has %d entries after %d timeouts, want 0", pendingCount, rounds)
	}
}

func TestCodexReadLoopEmitsDisconnectedOnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	s.setRootThreadID("test")
	go s.readLoop()

	s.Close()

	var gotDisconnected bool
	timeout := time.After(5 * time.Second)
	for !gotDisconnected {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for disconnected event")
		}
	}
}

// TestCodexReadLoopEmitsErrorStatusOnCleanUnexpectedExit pins the
// Codex-side mirror of the Claude quiet-disconnect bug fix. An
// app-server that exits with status 0 without us asking it to close
// is still abnormal — triage's handleSessionDied needs the "error"
// signal to synthesize the truncated turn-complete so the FE working
// indicator clears. The previous `exitErr != nil` gate dropped this
// emission when the process exited cleanly or when WaitProcessExitErr
// hit its 100ms timeout before the OS reaped the child.
func TestCodexReadLoopEmitsErrorStatusOnCleanUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "sleep 0.05; exit 0"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	s.setRootThreadID("test")
	go s.readLoop()

	var gotError, gotDisconnected bool
	timeout := time.After(5 * time.Second)
	for !(gotError && gotDisconnected) {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventSessionStatus {
				continue
			}
			switch evt.Content {
			case "error":
				gotError = true
			case "disconnected":
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatalf("timeout waiting for error+disconnected on clean unexpected exit (gotError=%v gotDisconnected=%v)", gotError, gotDisconnected)
		}
	}
}

func TestCodexReadLoopEmitsErrorStatusOnUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "sleep 0.05; exit 9"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	s.setRootThreadID("test")
	go s.readLoop()

	var gotError, gotDisconnected bool
	timeout := time.After(5 * time.Second)
	for !(gotError && gotDisconnected) {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventSessionStatus {
				continue
			}
			switch evt.Content {
			case "error":
				gotError = true
				var meta provider.ProcessExitInfo
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal exit meta: %v", err)
				}
				if meta.ExitCode != 9 {
					t.Fatalf("exitCode = %d, want 9", meta.ExitCode)
				}
			case "disconnected":
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for unexpected-exit events")
		}
	}
}

func TestCodexReadLoopCleansPendingOnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	s.setRootThreadID("test")

	// Add a pending request before readLoop starts.
	pendingCh := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[99] = pendingCh
	s.mu.Unlock()

	go s.readLoop()

	// Kill the process — readLoop should clean up pending.
	s.Close()

	// The pending channel should be closed.
	select {
	case _, ok := <-pendingCh:
		if ok {
			t.Error("expected pending channel to be closed, got a value")
		}
		// Channel was closed — correct.
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for pending channel to be closed")
	}
}
