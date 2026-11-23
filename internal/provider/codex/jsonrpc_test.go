package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestIsThreadNotFoundRequiresTheResumeErrorShape(t *testing.T) {
	t.Parallel()
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

// -- Session JSON-RPC over a fixture connection --

func TestWriteNotificationFormat(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)

	if err := fx.conn.writeNotification("initialized", nil); err != nil {
		t.Fatalf("writeNotification(initialized): %v", err)
	}
	if err := fx.conn.writeNotification("test/method", map[string]any{"key": "value"}); err != nil {
		t.Fatalf("writeNotification(test/method): %v", err)
	}

	bare := fx.nextWrite()
	if bare["jsonrpc"] != "2.0" || bare["method"] != "initialized" {
		t.Errorf("initialized frame = %v", bare)
	}
	if _, ok := bare["params"]; ok {
		t.Errorf("a notification without params must omit the key: %v", bare)
	}
	if _, ok := bare["id"]; ok {
		t.Errorf("a notification must carry no id: %v", bare)
	}
	withParams := fx.nextWrite()
	params, _ := withParams["params"].(map[string]any)
	if withParams["method"] != "test/method" || params["key"] != "value" {
		t.Errorf("test/method frame = %v", withParams)
	}
}

func TestDispatchLineResponse(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)

	wait, err := s.startRequest("test/method", nil)
	if err != nil {
		t.Fatalf("startRequest: %v", err)
	}
	id := int64(fx.nextRequest("test/method")["id"].(float64))
	fx.respond(id, map[string]any{"ok": true})

	result, err := wait(context.Background())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if string(result) != `{"ok":true}` {
		t.Errorf("result = %s, want {\"ok\":true}", result)
	}
}

func TestDispatchLineNotification(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s := fixtureSession(t, "t1", "codex-thread-1", func(evt provider.ProviderEvent) {
		events = append(events, evt)
	})

	line := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"codex-thread-1","turn":{"id":"turn-1"}}}`)
	feed(t, s, line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnStart)
	}
}

func TestDispatchLineInvalidJSON(t *testing.T) {
	t.Parallel()
	s := fixtureSession(t, "", "codex-thread-1", nil)
	// Should not panic — logs and returns.
	feed(t, s, []byte(`not valid json`))
}

func TestDispatchLineResponseNonIntegerID(t *testing.T) {
	t.Parallel()
	s := fixtureSession(t, "", "codex-thread-1", nil)
	// Float ID — Int64() fails, logged and returned.
	feed(t, s, []byte(`{"jsonrpc":"2.0","id":1.5,"result":{}}`))
}

func TestDispatchLineResponseNoMatchingPending(t *testing.T) {
	t.Parallel()
	s := fixtureSession(t, "", "codex-thread-1", nil)
	// Valid response but no pending request for id=999 — silently ignored.
	feed(t, s, []byte(`{"jsonrpc":"2.0","id":999,"result":{}}`))
}

func TestDispatchLineServerRequest(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s := fixtureSession(t, "t1", "codex-thread-1", func(evt provider.ProviderEvent) {
		events = append(events, evt)
	})

	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"item/commandExecution/requestApproval","params":{"threadId":"codex-thread-1","turnId":"turn-1","itemId":"item-1","command":"ls"}}`)
	feed(t, s, line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventApprovalRequest)
	}
}

func TestDispatchLineServerRequestNonIntegerID(t *testing.T) {
	t.Parallel()
	s := fixtureSession(t, "", "codex-thread-1", nil)
	// Server request with float ID — logged and returned.
	feed(t, s, []byte(`{"jsonrpc":"2.0","id":1.5,"method":"item/commandExecution/requestApproval","params":{"threadId":"codex-thread-1"}}`))
}

func TestCodexWriteResponse(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)

	if err := s.writeResponse(42, map[string]any{"ok": true}); err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
	frame := fx.nextWrite()
	result, _ := frame["result"].(map[string]any)
	if frame["id"] != float64(42) || result["ok"] != true {
		t.Errorf("response frame = %v", frame)
	}
}

func TestCodexSendRequestReturnsRPCError(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := s.sendRequest(context.Background(), "test/method", map[string]any{"key": "value"})
		errCh <- err
	}()
	id := fx.claimRequest()
	fx.feed([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"Method not found"}}`, id)))

	err := <-errCh
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("sendRequest error = %v, want the RPC error", err)
	}
}

func TestCodexSendRequestContextCancel(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := s.sendRequest(ctx, "test/method", nil)
	if err == nil {
		t.Fatal("expected context canceled error")
	}
	// The request reached the wire; its cancelled waiter answers nothing.
	fx.claimRequest()
}

func TestCodexSendRequestReturnsErrorWhenSessionStops(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := s.sendRequest(context.Background(), "test/method", map[string]any{"key": "value"})
		errCh <- err
	}()
	fx.claimRequest()

	if err := s.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected session stop error")
		}
		if !strings.Contains(err.Error(), "before request completed") {
			t.Fatalf("error = %v, want the stopped-before-completion message", err)
		}
	case <-time.After(fixtureWait):
		t.Fatal("timeout waiting for sendRequest to fail")
	}
}

// A response that arrives after its request timed out is dropped by the
// connection's dispatch, and the timed-out request leaves no pending entry.
func TestCodexSendRequestTimeoutDrainsLateResponse(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)
	s.requestTimeoutOverride = time.Millisecond

	const rounds = 10
	for i := 0; i < rounds; i++ {
		_, err := s.sendRequest(context.Background(), "test/method", map[string]any{"k": "v"})
		if !IsRequestTimeout(err, "test/method") {
			t.Fatalf("round %d: err = %v, want a timeout", i, err)
		}
		id := fx.claimRequest()
		fx.feed([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"too":"late"}}`, id)))
	}

	fx.conn.pendingMu.Lock()
	pendingCount := len(fx.conn.pending)
	fx.conn.pendingMu.Unlock()
	if pendingCount != 0 {
		t.Errorf("pending has %d entries after %d timeouts and late responses, want 0", pendingCount, rounds)
	}
}

func TestCodexReadLoopEmitsDisconnectedOnClose(t *testing.T) {
	t.Parallel()
	eventCh := make(chan provider.ProviderEvent, 100)
	s := fixtureSession(t, testThread, "codex-thread-1", func(evt provider.ProviderEvent) {
		eventCh <- evt
	})

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventSessionStatus && evt.Content == "error" {
				t.Fatalf("an asked-for close emitted an error status: %+v", evt)
			}
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				return
			}
		default:
			t.Fatal("Close returned before the disconnected status was emitted")
		}
	}
}

// An app-server that exits without being asked to is abnormal whatever its
// status: triage's handleSessionDied needs the "error" signal to synthesize
// the truncated turn-complete so the working indicator clears.
func TestCodexReadLoopEmitsErrorStatusOnUnexpectedExit(t *testing.T) {
	t.Parallel()
	exitErr := exec.Command("sh", "-c", "exit 9").Run()
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "clean exit", err: nil, wantCode: 0},
		{name: "exit status 9", err: exitErr, wantCode: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newConnFixture(t)
			var events []provider.ProviderEvent
			fx.session(testThread, "codex-thread-1", func(evt provider.ProviderEvent) {
				events = append(events, evt)
			})
			fx.pipe.exitErr = tc.err

			fx.exitAppServer()

			var gotError, gotDisconnected bool
			for _, evt := range events {
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
					if meta.ExitCode != tc.wantCode {
						t.Errorf("exitCode = %d, want %d", meta.ExitCode, tc.wantCode)
					}
				case "disconnected":
					gotDisconnected = true
				}
			}
			if !gotError || !gotDisconnected {
				t.Fatalf("events = %+v, want error and disconnected statuses", events)
			}
		})
	}
}

func TestCodexReadLoopCleansPendingOnExit(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := s.sendRequest(context.Background(), "test/method", nil)
		errCh <- err
	}()
	fx.claimRequest()

	fx.exitAppServer()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("a request in flight when the app-server exited must fail")
		}
	case <-time.After(fixtureWait):
		t.Fatal("the in-flight request was not released when the app-server exited")
	}
}

// isThreadWriterConflict reports whether err is a writer-ownership refusal.
func isThreadWriterConflict(err error) bool {
	var conflict *ThreadWriterConflictError
	return errors.As(err, &conflict)
}
