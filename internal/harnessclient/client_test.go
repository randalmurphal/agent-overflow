package harnessclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/transport"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCallSendsMethodByNameWithPositionalParams(t *testing.T) {
	backend := newFakeBackend(t)
	seen := make(chan transport.ClientFrame, 1)
	backend.handle = func(frame transport.ClientFrame) (json.RawMessage, *transport.FrameError) {
		seen <- frame
		return json.RawMessage(`{"ok":true}`), nil
	}
	client := backend.dial(t)

	result, err := client.Call(testContext(t), "HarnessSeed", map[string]any{"projects": []any{}}, 7)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(result) != `{"ok":true}` {
		t.Fatalf("result = %s", result)
	}

	frame := <-seen
	if frame.Method != "HarnessSeed" {
		t.Errorf("method = %q, want HarnessSeed", frame.Method)
	}
	if frame.MethodID != 0 {
		t.Errorf("methodId = %d, want 0 (a CLI has no generated bindings and must dispatch by name)", frame.MethodID)
	}
	if len(frame.Params) != 2 {
		t.Fatalf("params = %v, want 2 positional values", frame.Params)
	}
	if string(frame.Params[1]) != "7" {
		t.Errorf("second param = %s, want 7", frame.Params[1])
	}
	if frame.ID == "" {
		t.Error("rpc frame carried no correlation id")
	}
}

func TestCallRawForwardsEncodedParamsVerbatim(t *testing.T) {
	// The CLI's `rpc` verb takes JSON text off the command line. Encoding
	// it a second time would turn an object into a string.
	backend := newFakeBackend(t)
	seen := make(chan transport.ClientFrame, 1)
	backend.handle = func(frame transport.ClientFrame) (json.RawMessage, *transport.FrameError) {
		seen <- frame
		return json.RawMessage(`null`), nil
	}
	client := backend.dial(t)

	if _, err := client.CallRaw(testContext(t), "HarnessEmit", []json.RawMessage{
		json.RawMessage(`"thread:updated"`),
		json.RawMessage(`{"id":"t1"}`),
	}); err != nil {
		t.Fatalf("CallRaw: %v", err)
	}
	frame := <-seen
	if string(frame.Params[1]) != `{"id":"t1"}` {
		t.Fatalf("param = %s, want the object as given", frame.Params[1])
	}
}

func TestCallSurfacesTheServersErrorCode(t *testing.T) {
	backend := newFakeBackend(t)
	backend.handle = func(frame transport.ClientFrame) (json.RawMessage, *transport.FrameError) {
		return nil, &transport.FrameError{Code: transport.ErrCodeMethodNotFound, Message: "no such method"}
	}
	client := backend.dial(t)

	_, err := client.Call(testContext(t), "NoSuchMethod")
	if err == nil {
		t.Fatal("Call succeeded against a method_not_found")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error %v is not an *RPCError", err)
	}
	if rpcErr.Code != transport.ErrCodeMethodNotFound {
		t.Errorf("code = %q, want %q", rpcErr.Code, transport.ErrCodeMethodNotFound)
	}
	if rpcErr.Method != "NoSuchMethod" {
		t.Errorf("method = %q, want the method that failed", rpcErr.Method)
	}
}

func TestWaitForEventScansHistoryBeforeWaiting(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushEvent(t, "harness:mock", map[string]any{"kind": "registered"})

	// Let the read loop file it, so the wait below can only be answered
	// from history — the race a fast backend wins against a slow test.
	waitForCount(t, client, "harness:mock", 1)

	event, err := client.WaitForEvent(testContext(t), "harness:mock", nil)
	if err != nil {
		t.Fatalf("WaitForEvent: %v", err)
	}
	if !strings.Contains(string(event.Data), "registered") {
		t.Fatalf("event data = %s", event.Data)
	}
}

func TestWaitForEventConsumesSoTwoWaitsSeeTwoOccurrences(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushEvent(t, "provider:turn_completed", map[string]any{"turn": 1})
	backend.pushEvent(t, "provider:turn_completed", map[string]any{"turn": 2})
	waitForCount(t, client, "provider:turn_completed", 2)

	first, err := client.WaitForEvent(testContext(t), "provider:turn_completed", nil)
	if err != nil {
		t.Fatalf("first wait: %v", err)
	}
	second, err := client.WaitForEvent(testContext(t), "provider:turn_completed", nil)
	if err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if first.Seq == second.Seq {
		t.Fatalf("both waits observed the same event (seq %d); consumption is what multi-turn assertions depend on", first.Seq)
	}
}

func TestWaitForEventHonoursThePredicate(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushEvent(t, "harness:mock", map[string]any{"kind": "registered"})
	backend.pushEvent(t, "harness:mock", map[string]any{"kind": "scenario_done"})
	waitForCount(t, client, "harness:mock", 2)

	event, err := client.WaitForEvent(testContext(t), "harness:mock", func(ev Event) bool {
		return strings.Contains(string(ev.Data), "scenario_done")
	})
	if err != nil {
		t.Fatalf("WaitForEvent: %v", err)
	}
	if !strings.Contains(string(event.Data), "scenario_done") {
		t.Fatalf("matched the wrong event: %s", event.Data)
	}
	// The unmatched earlier event must still be waitable.
	if _, err := client.WaitForEvent(testContext(t), "harness:mock", nil); err != nil {
		t.Fatalf("the predicate consumed a non-matching event: %v", err)
	}
}

func TestWaitForEventDeliversAnEventThatArrivesLater(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		backend.pushEvent(t, "workflow:item-state", map[string]any{"state": "done"})
	}()
	event, err := client.WaitForEvent(testContext(t), "workflow:item-state", nil)
	if err != nil {
		t.Fatalf("WaitForEvent: %v", err)
	}
	if !strings.Contains(string(event.Data), "done") {
		t.Fatalf("event data = %s", event.Data)
	}
}

func TestWaitForEventTimeoutNamesRecentChannels(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushEvent(t, "harness:mock", map[string]any{"kind": "registered"})
	backend.pushEvent(t, "harness:mock", map[string]any{"kind": "turn_started"})
	backend.pushEvent(t, "thread:updated", map[string]any{"id": "t1"})
	waitForCount(t, client, "harness:mock", 2)
	waitForCount(t, client, "thread:updated", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := client.WaitForEvent(ctx, "never:emitted", nil)
	if err == nil {
		t.Fatal("wait for an unemitted channel succeeded")
	}
	for _, want := range []string{"harness:mock", "thread:updated"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("timeout error omits recent channel %s: %v", want, err)
		}
	}
	// A repeat says nothing the first mention did not.
	if strings.Count(err.Error(), "harness:mock") != 1 {
		t.Fatalf("timeout error repeats a channel: %v", err)
	}
}

func TestBatchFrameDispatchesEveryCarriedEvent(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushBatch(t, "harness:mock", "harness:mock", "thread:updated")
	waitForCount(t, client, "harness:mock", 2)

	if got := client.Count("thread:updated", nil); got != 1 {
		t.Fatalf("thread:updated count = %d, want 1", got)
	}
}

func TestPingFramesAreTolerated(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushPing(t)
	backend.pushEvent(t, "harness:mock", map[string]any{"kind": "registered"})

	if _, err := client.WaitForEvent(testContext(t), "harness:mock", nil); err != nil {
		t.Fatalf("a keepalive ping broke the read loop: %v", err)
	}
	if got := client.Count("", nil); got != 0 {
		t.Fatalf("a ping was filed as an event (%d on the empty channel)", got)
	}
}

func TestClearDropsRememberedEvents(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushEvent(t, "harness:mock", map[string]any{"kind": "registered"})
	waitForCount(t, client, "harness:mock", 1)

	client.Clear()
	if got := client.Count("harness:mock", nil); got != 0 {
		t.Fatalf("count after Clear = %d, want 0", got)
	}
	if got := len(client.Events()); got != 0 {
		t.Fatalf("Events after Clear = %d, want 0", got)
	}
}

func TestCountIgnoresConsumption(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushEvent(t, "harness:mock", map[string]any{"kind": "registered"})
	waitForCount(t, client, "harness:mock", 1)
	if _, err := client.WaitForEvent(testContext(t), "harness:mock", nil); err != nil {
		t.Fatalf("WaitForEvent: %v", err)
	}
	// The absence half of an assertion has to see everything that
	// happened, not everything nobody waited on.
	if got := client.Count("harness:mock", nil); got != 1 {
		t.Fatalf("count after a consuming wait = %d, want 1", got)
	}
}

func TestSubscribeAndReplayReachTheServer(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	if err := client.Subscribe(testContext(t), "harness:mock", "harness:replay"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := client.Replay(testContext(t), map[string]uint64{"harness:mock": 4}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	subs := backend.subscriptions()
	if len(subs) != 2 || subs[0] != "harness:mock" {
		t.Fatalf("subscriptions = %v", subs)
	}
	if len(backend.replayRequests()) != 1 {
		t.Fatalf("replay requests = %v, want exactly one", backend.replayRequests())
	}
}

func TestClosedConnectionFailsAWaitAndLaterCalls(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	// Force the server side down under a wait.
	conn := backend.waitConn(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = conn.CloseNow()
	}()
	if _, err := client.WaitForEvent(testContext(t), "never:emitted", nil); err == nil {
		t.Fatal("a wait survived the connection closing")
	}
	if _, err := client.Call(testContext(t), "HarnessInfo"); err == nil {
		t.Fatal("a call on a dead connection succeeded")
	}
}

func TestDialRefusedTokenSaysSoWithoutLeakingIt(t *testing.T) {
	backend := newFakeBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := DialURL(ctx, strings.Replace(backend.wsURL(), backend.token, "wrong-secret", 1), Options{})
	if err == nil {
		t.Fatal("dial with a wrong token succeeded")
	}
	if strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("the failed-dial error quotes the token: %v", err)
	}
	if !strings.Contains(err.Error(), "token was refused") {
		t.Fatalf("error does not explain a 404: %v", err)
	}
}

func TestInfoDecodesTheTypedResult(t *testing.T) {
	backend := newFakeBackend(t)
	backend.handle = func(frame transport.ClientFrame) (json.RawMessage, *transport.FrameError) {
		if frame.Method != "HarnessInfo" {
			t.Errorf("method = %q", frame.Method)
		}
		return json.RawMessage(`{"version":"dev","pid":42,"dbPath":"/tmp/x/agent-overflow.db"}`), nil
	}
	client := backend.dial(t)
	info, err := client.Info(testContext(t))
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.PID != 42 || info.DBPath != "/tmp/x/agent-overflow.db" {
		t.Fatalf("info = %+v", info)
	}
}

// waitForCount blocks until the client's log holds n events on a
// channel. Every event assertion here needs the read loop to have filed
// what the fake backend already wrote, and a sleep would be the flaky
// way to say that.
func waitForCount(t *testing.T, client *Client, channel string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if client.Count(channel, nil) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("client never received %d events on %s (has %d)", n, channel, client.Count(channel, nil))
}

// newLogOnlyClient is a Client with just the event-log machinery wired:
// dispatch, the waiter registry, and the log itself need no connection,
// and building one without a server keeps these cases about the log.
func newLogOnlyClient(logCap int) *Client {
	return &Client{
		pending:   map[string]*pendingCall{},
		replays:   map[string]chan struct{}{},
		waiters:   map[int]*waiter{},
		listeners: map[int]func(Event){},
		logCap:    logCap,
		readDone:  make(chan struct{}),
	}
}

// At the cap the log sheds a CHUNK. Shifting by one per arrival would be
// O(cap) on every event — quadratic over a sustained stream, on the read
// loop, under the mutex.
func TestEventLogShedsAChunkAtItsCap(t *testing.T) {
	const logCap = 8

	// The event that first exceeds the cap costs a whole chunk, not one
	// entry: one-at-a-time eviction would leave the log sitting exactly
	// at the cap here, and would go on paying that memmove per event.
	atCap := newLogOnlyClient(logCap)
	for i := 0; i <= logCap; i++ {
		atCap.dispatch(Event{Channel: "harness:perf", Seq: uint64(i)})
	}
	if got, want := len(atCap.Events()), logCap+1-logCap/logShedDivisor; got != want {
		t.Fatalf("log holds %d events after one shed, want %d", got, want)
	}

	client := newLogOnlyClient(logCap)
	for i := 0; i < 40; i++ {
		client.dispatch(Event{Channel: "harness:perf", Seq: uint64(i)})
	}
	events := client.Events()
	if len(events) > logCap {
		t.Fatalf("log holds %d events over a cap of %d", len(events), logCap)
	}
	if len(events) == 0 || events[len(events)-1].Seq != 39 {
		t.Fatalf("newest event = %+v, want seq 39", events[len(events)-1:])
	}
	// What survives is the newest contiguous run: a shed drops from the
	// front and never punches a hole.
	for i, event := range events {
		want := events[0].Seq + uint64(i)
		if event.Seq != want {
			t.Fatalf("event %d has seq %d, want %d (the retained run must be contiguous)", i, event.Seq, want)
		}
	}
}

// Two identical waits are ordered: the longest-waiting one consumes the
// event. Map iteration is randomized, so this is a property the dispatch
// has to enforce rather than one it inherits.
func TestTheLongestWaitingWaiterConsumesTheEvent(t *testing.T) {
	// Repeated, because a randomized map with two entries picks the right
	// one half the time by accident.
	for attempt := 0; attempt < 50; attempt++ {
		client := newLogOnlyClient(defaultEventLogCap)
		first := &waiter{channel: "thread:updated", out: make(chan Event, 1)}
		second := &waiter{channel: "thread:updated", out: make(chan Event, 1)}
		client.mu.Lock()
		client.nextHook++
		client.waiters[client.nextHook] = first
		client.nextHook++
		client.waiters[client.nextHook] = second
		client.mu.Unlock()

		client.dispatch(Event{Channel: "thread:updated", Seq: 1})
		select {
		case <-first.out:
		default:
			t.Fatalf("attempt %d: the event went to the later waiter", attempt)
		}
		if len(second.out) != 0 {
			t.Fatalf("attempt %d: both waiters were served one event", attempt)
		}
	}
}
