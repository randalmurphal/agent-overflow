package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// integrationStub is a small, focused receiver that exercises the five
// signature shapes the dispatcher must handle end-to-end across the
// wire: simple call, error return, multi-return, ctx-needing, slice
// param. It mirrors the shapes in dispatcher_test.go's fakeApp but is
// scoped to this file so the integration test owns its observability
// hooks (in-flight count, drain latch) without polluting the unit test
// surface.
type integrationStub struct {
	// inFlight tracks how many SlowOp calls are currently sleeping. The
	// shutdown-drains test waits for inFlight to flip back to 0 to prove
	// the parent shutdown blocked until handlers returned.
	inFlight atomic.Int32

	// release lets the test gate SlowOp's return so the goroutine is
	// guaranteed to be in flight when shutdown begins.
	releaseMu sync.Mutex
	release   chan struct{}
}

// SimpleCall is the simplest input -> output shape.
func (s *integrationStub) SimpleCall(name string) string {
	return "hello " + name
}

// MaybeFail returns (T, error). The error path proves
// ErrCodeMethodError surfaces through the wire with the receiver's own
// message preserved.
func (s *integrationStub) MaybeFail(unhappy bool) (string, error) {
	if unhappy {
		return "", errors.New("integrationStub unhappy")
	}
	return "ok", nil
}

// MultiReturn returns (T1, T2) without a trailing error so the JSON-
// array fallback path is exercised.
func (s *integrationStub) MultiReturn(input string) (string, int) {
	return input, len(input)
}

// CtxLabel needs a context.Context as its first parameter so the
// dispatcher's NeedsContext branch is exercised end-to-end.
func (s *integrationStub) CtxLabel(ctx context.Context, label string) string {
	if ctx == nil {
		return "no-ctx"
	}
	return "ctx:" + label
}

// JoinLines accepts a slice param — the most common production shape.
func (s *integrationStub) JoinLines(lines []string) string {
	return strings.Join(lines, ",")
}

// SlowOp blocks until the test releases it. The shutdown-drains test
// uses this method to guarantee an RPC is mid-flight when Shutdown is
// invoked, then verifies Shutdown waits for it.
func (s *integrationStub) SlowOp() string {
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)

	s.releaseMu.Lock()
	ch := s.release
	s.releaseMu.Unlock()

	if ch != nil {
		<-ch
	}
	return "released"
}

// armRelease installs a fresh release channel; the test closes it via
// the returned func when ready to let SlowOp return.
func (s *integrationStub) armRelease() func() {
	s.releaseMu.Lock()
	defer s.releaseMu.Unlock()
	s.release = make(chan struct{})
	ch := s.release
	return func() {
		close(ch)
	}
}

// integrationFixture wires a real Server bound on a real ephemeral
// loopback port with the stub receiver registered. Token is fixed so
// the dial code stays simple; any test that wants to exercise token
// negotiation can boot its own.
type integrationFixture struct {
	t    *testing.T
	srv  *Server
	stub *integrationStub
	bus  *EventBus
	addr string
}

func newIntegrationFixture(t *testing.T) *integrationFixture {
	t.Helper()
	d := NewDispatcher()
	stub := &integrationStub{}
	if _, err := d.Register(stub, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	bus := NewEventBus(64)
	srv, err := New(Config{
		Dispatcher: d,
		EventBus:   bus,
		Token:      "integration-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort cleanup so a failing test doesn't leak the listener.
		// The shutdown-drains test calls Shutdown directly and relies on
		// idempotency to make this Cleanup a no-op.
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})
	return &integrationFixture{
		t:    t,
		srv:  srv,
		stub: stub,
		bus:  bus,
		addr: srv.Addr(),
	}
}

func (f *integrationFixture) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	url := "ws://" + f.addr + "/ws?token=integration-token"
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// callRPC is a small synchronous helper: send a single RPC frame, read
// the matching response, return it. Caller arranges no event frames
// arrive concurrently or filters them in a separate reader.
func callRPC(t *testing.T, conn *websocket.Conn, methodName string, params ...any) ServerFrame {
	t.Helper()
	rawParams := make([]json.RawMessage, len(params))
	for i, p := range params {
		buf, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal param %d: %v", i, err)
		}
		rawParams[i] = buf
	}
	frame := ClientFrame{
		Type:     frameTypeRPC,
		ID:       fmt.Sprintf("req-%s-%d", methodName, time.Now().UnixNano()),
		MethodID: fnvHash("main.App." + methodName),
		Method:   methodName,
		Params:   rawParams,
	}
	buf, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var resp ServerFrame
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Type == frameTypeRPC && resp.ID == frame.ID {
			return resp
		}
	}
}

// TestIntegration_FiveRPCsRoundTripAcrossWire exercises one method per
// signature shape end-to-end: the boot path, dispatcher reflection,
// JSON wire encoding, the WS read+write goroutines, and the response
// match-up. A regression in any of those layers fails this test.
func TestIntegration_FiveRPCsRoundTripAcrossWire(t *testing.T) {
	f := newIntegrationFixture(t)
	conn := f.dial(t)

	// 1. Simple call: input string, output string.
	resp := callRPC(t, conn, "SimpleCall", "wire")
	if resp.Error != nil {
		t.Fatalf("SimpleCall error: %+v", resp.Error)
	}
	if string(resp.Result) != `"hello wire"` {
		t.Fatalf("SimpleCall result = %s, want \"hello wire\"", string(resp.Result))
	}

	// 2. Error return surfacing: receiver's error reaches the wire as
	//    ErrCodeMethodError. The integration fixture connects via
	//    127.0.0.1, so the dispatcher treats the caller as loopback —
	//    the methodErr.Error() text comes through directly. LAN peers
	//    get the redacted "method failed (id: ...)" envelope (covered
	//    by the conn-level tests against a non-loopback dialler).
	resp = callRPC(t, conn, "MaybeFail", true)
	if resp.Error == nil {
		t.Fatalf("MaybeFail(true) expected error frame, got result %s", string(resp.Result))
	}
	if resp.Error.Code != ErrCodeMethodError {
		t.Fatalf("MaybeFail error code = %s, want %s", resp.Error.Code, ErrCodeMethodError)
	}
	if resp.Error.Message != "integrationStub unhappy" {
		t.Fatalf("MaybeFail loopback caller should see receiver text, got %q", resp.Error.Message)
	}
	// Happy path of the same method.
	resp = callRPC(t, conn, "MaybeFail", false)
	if resp.Error != nil {
		t.Fatalf("MaybeFail(false) error: %+v", resp.Error)
	}
	if string(resp.Result) != `"ok"` {
		t.Fatalf("MaybeFail(false) result = %s, want \"ok\"", string(resp.Result))
	}

	// 3. Multi-return without trailing error: JSON-array shape.
	resp = callRPC(t, conn, "MultiReturn", "abc")
	if resp.Error != nil {
		t.Fatalf("MultiReturn error: %+v", resp.Error)
	}
	if string(resp.Result) != `["abc",3]` {
		t.Fatalf("MultiReturn result = %s, want [\"abc\",3]", string(resp.Result))
	}

	// 4. Context injection: receiver sees a non-nil ctx because the
	//    dispatcher threads its own ctx into the call. The wire never
	//    sees the ctx slot, so callRPC's params skip it.
	resp = callRPC(t, conn, "CtxLabel", "label")
	if resp.Error != nil {
		t.Fatalf("CtxLabel error: %+v", resp.Error)
	}
	if string(resp.Result) != `"ctx:label"` {
		t.Fatalf("CtxLabel result = %s, want \"ctx:label\"", string(resp.Result))
	}

	// 5. Slice param: receiver gets the JSON array decoded into []string.
	resp = callRPC(t, conn, "JoinLines", []string{"a", "b", "c"})
	if resp.Error != nil {
		t.Fatalf("JoinLines error: %+v", resp.Error)
	}
	if string(resp.Result) != `"a,b,c"` {
		t.Fatalf("JoinLines result = %s, want \"a,b,c\"", string(resp.Result))
	}
}

// TestIntegration_ReplayRoundTrip pins the per-channel replay contract
// over the wire: events emitted before a client connects are replayed
// in order on a `replay` frame, with seq stamped per-channel.
func TestIntegration_ReplayRoundTrip(t *testing.T) {
	f := newIntegrationFixture(t)

	// Emit four events into two channels BEFORE any client connects.
	// The bus's per-channel seq is independent so each channel's first
	// event has seq=1.
	for i := 0; i < 3; i++ {
		if _, err := f.bus.Emit("ch:a", map[string]int{"i": i}); err != nil {
			t.Fatalf("emit ch:a: %v", err)
		}
	}
	if _, err := f.bus.Emit("ch:b", "single"); err != nil {
		t.Fatalf("emit ch:b: %v", err)
	}

	conn := f.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Ask the server for everything missed since seq 0 in both channels.
	frame := ClientFrame{
		Type: frameTypeReplay,
		LastSeqByChannel: map[string]uint64{
			"ch:a": 0,
			"ch:b": 0,
		},
	}
	buf, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal replay: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatalf("ws write: %v", err)
	}

	// Drain four events, group by channel, verify per-channel seq order.
	got := map[string][]ServerFrame{}
	for total := 0; total < 4; {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var fr ServerFrame
		if err := json.Unmarshal(raw, &fr); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if fr.Type != frameTypeEvent {
			continue
		}
		got[fr.Channel] = append(got[fr.Channel], fr)
		total++
	}

	chA := got["ch:a"]
	if len(chA) != 3 {
		t.Fatalf("ch:a got %d events, want 3", len(chA))
	}
	for i, evt := range chA {
		if evt.Seq != uint64(i+1) {
			t.Fatalf("ch:a event %d seq = %d, want %d", i, evt.Seq, i+1)
		}
		if evt.Gap {
			t.Fatalf("ch:a event %d unexpectedly gap-flagged", i)
		}
	}

	chB := got["ch:b"]
	if len(chB) != 1 {
		t.Fatalf("ch:b got %d events, want 1", len(chB))
	}
	if chB[0].Seq != 1 {
		t.Fatalf("ch:b event seq = %d, want 1 (per-channel seq is independent)", chB[0].Seq)
	}
	var payload string
	if err := json.Unmarshal(chB[0].Data, &payload); err != nil {
		t.Fatalf("ch:b payload decode: %v", err)
	}
	if payload != "single" {
		t.Fatalf("ch:b payload = %q, want \"single\"", payload)
	}
}

// TestIntegration_ShutdownDrainsInflightRPCs proves Server.Shutdown
// blocks until in-flight RPC handlers return their responses to the
// wire. Without the drain, the WS would close out from under the
// handler and the client would observe a hung response.
func TestIntegration_ShutdownDrainsInflightRPCs(t *testing.T) {
	f := newIntegrationFixture(t)
	conn := f.dial(t)
	release := f.stub.armRelease()

	// Fire SlowOp on its own goroutine — it will sit inside the stub
	// until the test releases it.
	frame := ClientFrame{
		Type:     frameTypeRPC,
		ID:       "slow-1",
		MethodID: fnvHash("main.App.SlowOp"),
	}
	buf, _ := json.Marshal(frame)
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := conn.Write(writeCtx, websocket.MessageText, buf); err != nil {
		writeCancel()
		t.Fatalf("ws write: %v", err)
	}
	writeCancel()

	// Wait for SlowOp to be on the server-side.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.stub.inFlight.Load() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if f.stub.inFlight.Load() != 1 {
		t.Fatalf("SlowOp not observed in-flight before shutdown (inFlight=%d)", f.stub.inFlight.Load())
	}

	// Begin Shutdown on its own goroutine. We'll release the in-flight
	// RPC after Shutdown has actually started so the test proves the
	// drain waits for the handler rather than tearing the WS out from
	// under it.
	shutdownDone := make(chan error, 1)
	go func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		shutdownDone <- f.srv.Shutdown(shutCtx)
	}()

	// Wait until Shutdown has flipped the shutDown flag. shutDown is
	// set as the very first action inside stopOnce.Do, before
	// rootCancel, before http.Server.Shutdown. Polling for it gives a
	// deterministic "Shutdown has begun" signal without timer-based
	// hacks. The poll interval is short relative to typical shutdown
	// transitions, and the deadline guards a regression that would
	// otherwise hang the test.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !f.srv.shutDown.Load() {
		time.Sleep(1 * time.Millisecond)
	}
	if !f.srv.shutDown.Load() {
		t.Fatal("Shutdown never set shutDown flag within deadline")
	}
	if f.stub.inFlight.Load() != 1 {
		t.Fatalf("Shutdown abandoned in-flight handler before drain (inFlight=%d)", f.stub.inFlight.Load())
	}
	release()

	// Wait for Shutdown to complete. After it returns, the in-flight
	// handler MUST have finished (drained) — that's the contract.
	select {
	case err := <-shutdownDone:
		if err != nil {
			// A clean drain returns nil; http.Server.Shutdown returns
			// ErrServerClosed on subsequent calls but Server.Shutdown
			// is idempotent and the first call should return nil.
			t.Fatalf("Shutdown returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return within 5s")
	}

	if got := f.stub.inFlight.Load(); got != 0 {
		t.Fatalf("inFlight after Shutdown = %d, want 0 (handler must have returned)", got)
	}

	// Idempotent: a second Shutdown is a clean no-op.
	idemCtx, c := context.WithTimeout(context.Background(), 1*time.Second)
	defer c()
	if err := f.srv.Shutdown(idemCtx); err != nil {
		t.Fatalf("second Shutdown should be a no-op, got %v", err)
	}
}
