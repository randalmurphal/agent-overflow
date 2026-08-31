package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"

	"github.com/coder/websocket"
)

// serverFixture wires a real listener + dispatcher + bus so tests
// exercise the same boot path production uses. Caller invokes Close
// via t.Cleanup; tests that crash midway leak the goroutine, which
// the test runner reports as still-running on completion.
type serverFixture struct {
	t   *testing.T
	srv *Server
	app *fakeApp
	bus *EventBus
}

func newServerFixture(t *testing.T) *serverFixture {
	return newServerFixtureWith(t, nil)
}

// newServerFixtureWith lets a test adjust the server Config (e.g. the
// keepalive timing knobs) before Start.
func newServerFixtureWith(t *testing.T, mutate func(*Config)) *serverFixture {
	t.Helper()
	d := NewDispatcher()
	app := &fakeApp{}
	if _, err := d.Register(app, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	bus := NewEventBus(20)
	cfg := Config{
		Dispatcher: d,
		EventBus:   bus,
		Token:      "test-token",
	}
	if mutate != nil {
		mutate(&cfg)
		// A mutate may swap the bus (ring capacity is a per-bus knob);
		// the fixture must hand back the one the server actually runs.
		bus = cfg.EventBus
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})
	return &serverFixture{t: t, srv: srv, app: app, bus: bus}
}

func (f *serverFixture) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	url := "ws://" + f.srv.Addr() + "/ws?token=test-token"
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// rpc sends an RPC request and returns the matching response frame.
// Doesn't filter event frames — caller arranges that no events arrive
// during the test or buffers them separately.
func (f *serverFixture) rpc(t *testing.T, conn *websocket.Conn, methodID uint32, methodName string, params ...any) ServerFrame {
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
		ID:       fmt.Sprintf("req-%d", time.Now().UnixNano()),
		MethodID: methodID,
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

func TestServer_RPCRoundTrip(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	// Greet method via name lookup.
	resp := f.rpc(t, conn, 0, "Greet", "world")
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	if string(resp.Result) != `"hello, world"` {
		t.Fatalf("unexpected result: %s", string(resp.Result))
	}

	// Same method via ID lookup. fakeApp registers under main.App.Greet.
	id := fnvHash("main.App.Greet")
	resp = f.rpc(t, conn, id, "", "again")
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	if string(resp.Result) != `"hello, again"` {
		t.Fatalf("unexpected result: %s", string(resp.Result))
	}
}

func TestServer_AuthRequiresToken(t *testing.T) {
	f := newServerFixture(t)
	url := "ws://" + f.srv.Addr() + "/ws"
	_, _, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatalf("dial without token should fail")
	}
}

func TestServer_AuthRejectsWrongToken(t *testing.T) {
	f := newServerFixture(t)
	url := "ws://" + f.srv.Addr() + "/ws?token=wrong"
	_, _, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatalf("dial with wrong token should fail")
	}
}

func TestServer_BootstrapEndpoint(t *testing.T) {
	f := newServerFixture(t)
	resp, err := http.Get(fmt.Sprintf("http://%s/bootstrap.json?t=test-token", f.srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("bootstrap status: %d", resp.StatusCode)
	}
	var b Bootstrap
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	if b.Token != "test-token" {
		t.Fatalf("bootstrap token wrong: %s", b.Token)
	}
	if !strings.HasPrefix(b.WSURL, "ws://") || !strings.HasSuffix(b.WSURL, "/ws") {
		t.Fatalf("bootstrap WS URL malformed: %s", b.WSURL)
	}
	// The wsUrl host MUST be derived from the request, not from the
	// server's bind. Even on a 0.0.0.0 bind, hitting bootstrap from a
	// real client gives that client's resolved host back.
	if !strings.Contains(b.WSURL, f.srv.Addr()) {
		t.Fatalf("wsUrl should reflect request Host, got %q (server addr %q)", b.WSURL, f.srv.Addr())
	}
	if b.Remote {
		t.Fatal("loopback bootstrap marked remote")
	}
}

func TestServer_BootstrapRemoteUsesPeerLocality(t *testing.T) {
	f := newServerFixture(t)

	for _, tc := range []struct {
		name       string
		remoteAddr string
		wantRemote bool
	}{
		{name: "loopback", remoteAddr: "127.0.0.1:54321", wantRemote: false},
		{name: "non-loopback", remoteAddr: "192.168.1.25:54321", wantRemote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.test/bootstrap.json?t=test-token", nil)
			req.RemoteAddr = tc.remoteAddr
			recorder := httptest.NewRecorder()

			f.srv.handleBootstrap(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			var bootstrap Bootstrap
			if err := json.NewDecoder(recorder.Body).Decode(&bootstrap); err != nil {
				t.Fatalf("decode bootstrap: %v", err)
			}
			if bootstrap.Remote != tc.wantRemote {
				t.Fatalf("Remote = %v, want %v", bootstrap.Remote, tc.wantRemote)
			}
		})
	}
}

func TestDispatcher_WorkflowReadsAreRemoteSafe(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&privilegedApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	for _, name := range []string{
		"WorkflowListItems",
		"WorkflowGetItem",
		"WorkflowListItemCosts",
		"WorkflowListDefinitions",
		"WorkflowGetJobNotes",
		"WorkflowGetEngineState",
	} {
		if _, frameErr := d.ResolveForOrigin(0, name, false); frameErr != nil {
			t.Errorf("non-loopback resolve %s: %+v", name, frameErr)
		}
	}

	if _, frameErr := d.ResolveForOrigin(0, "WorkflowSetGlobalPause", false); frameErr == nil {
		t.Fatal("non-loopback peer resolved WorkflowSetGlobalPause mutation")
	} else if frameErr.Code != ErrCodeMethodNotFound {
		t.Fatalf("WorkflowSetGlobalPause error code = %q, want %q", frameErr.Code, ErrCodeMethodNotFound)
	}
}

func TestServer_BootstrapReadinessGate(t *testing.T) {
	d := NewDispatcher()
	bus := NewEventBus(20)
	srv, err := New(Config{
		Dispatcher:               d,
		EventBus:                 bus,
		Token:                    "test-token",
		RequireReadyForBootstrap: true,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})

	resp, err := http.Get(fmt.Sprintf("http://%s/bootstrap.json?t=test-token", srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("bootstrap status before ready = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if srv.Ready() {
		t.Fatal("server reported ready before MarkReady")
	}

	badTokenResp, err := http.Get(fmt.Sprintf("http://%s/bootstrap.json?t=wrong", srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	_ = badTokenResp.Body.Close()
	if badTokenResp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad token status before ready = %d, want %d", badTokenResp.StatusCode, http.StatusNotFound)
	}

	srv.MarkReady()
	if !srv.Ready() {
		t.Fatal("server did not report ready after MarkReady")
	}
	resp, err = http.Get(fmt.Sprintf("http://%s/bootstrap.json?t=test-token", srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap status after ready = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var b Bootstrap
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	if b.Token != "test-token" {
		t.Fatalf("bootstrap token wrong: %s", b.Token)
	}
}

func TestServer_BootstrapStartupFailureBeatsReadinessGate(t *testing.T) {
	d := NewDispatcher()
	bus := NewEventBus(20)
	srv, err := New(Config{
		Dispatcher:               d,
		EventBus:                 bus,
		Token:                    "test-token",
		RequireReadyForBootstrap: true,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})

	srv.MarkStartupFailed()
	resp, err := http.Get(fmt.Sprintf("http://%s/bootstrap.json?t=test-token", srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("bootstrap status after startup failure = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

func TestServer_BootstrapRejectsBadToken(t *testing.T) {
	f := newServerFixture(t)
	resp, err := http.Get(fmt.Sprintf("http://%s/bootstrap.json?t=wrong", f.srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Indistinguishable from "no such path" — 404, not 401, so a LAN
	// scanner can't fingerprint the agent-overflow server.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestServer_BootstrapRejectsEmptyToken(t *testing.T) {
	f := newServerFixture(t)
	resp, err := http.Get(fmt.Sprintf("http://%s/bootstrap.json", f.srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestServer_EventLiveDelivery(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	// Wait for pump-events goroutine to attach via the public
	// SubscriberCount accessor — no reaching into private fields.
	if !waitFor(func() bool {
		return f.bus.SubscriberCount() >= 1
	}, 500*time.Millisecond) {
		t.Fatalf("server never registered subscriber")
	}

	want := map[string]string{"hello": "world"}
	f.bus.Emit("ch1", want)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var frame ServerFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if frame.Type != frameTypeEvent {
			continue
		}
		if frame.Channel != "ch1" {
			t.Fatalf("wrong channel: %s", frame.Channel)
		}
		var got map[string]string
		if err := json.Unmarshal(frame.Data, &got); err != nil {
			t.Fatalf("decode event payload: %v", err)
		}
		if got["hello"] != "world" {
			t.Fatalf("payload lost: %+v", got)
		}
		if frame.Seq != 1 {
			t.Fatalf("first event should be seq=1, got %d", frame.Seq)
		}
		return
	}
}

func TestServer_EventSubscriptionFiltersLiveDelivery(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	frame, err := json.Marshal(ClientFrame{
		Type:     frameTypeSubscribe,
		Channels: []string{"notification:send"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool {
		return f.bus.ChannelSubscriberCount("notification:send") == 1
	}, 500*time.Millisecond) {
		t.Fatal("server never applied event subscription")
	}
	if _, err := f.bus.Emit("provider:item_event", "ignored"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bus.Emit("notification:send", "wanted"); err != nil {
		t.Fatal(err)
	}
	var got ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != frameTypeEvent || got.Channel != "notification:send" {
		t.Fatalf("filtered event = %#v, want notification:send event", got)
	}
}

// replayResult is what drainReplay observed on the wire between the
// replay request and its completion marker.
type replayResult struct {
	// events are the replayed entries in wire order, flattened across
	// batch frames.
	events []batchEventEntry
	// batchFrames / eventFrames count the wire frames the entries
	// arrived in, so a test can pin the batching contract itself.
	batchFrames int
	eventFrames int
}

// requestReplay writes a replay frame and drains the response up to and
// including the completion marker. Both frame shapes are accepted —
// handleReplay chunks into batch frames but a chunk of exactly one
// event falls through to a plain event frame.
func requestReplay(t *testing.T, conn *websocket.Conn, lastSeqByChannel map[string]uint64) replayResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	buf, err := json.Marshal(ClientFrame{
		Type:             frameTypeReplay,
		LastSeqByChannel: lastSeqByChannel,
	})
	if err != nil {
		t.Fatalf("marshal replay frame: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatalf("ws write: %v", err)
	}

	var out replayResult
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var probe ServerFrame
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		switch probe.Type {
		case frameTypeReplay:
			return out
		case frameTypeEvent:
			out.eventFrames++
			out.events = append(out.events, batchEventEntry{
				Channel: probe.Channel,
				Seq:     probe.Seq,
				Data:    probe.Data,
				Gap:     probe.Gap,
			})
		case frameTypeBatch:
			var batch batchFrame
			if err := json.Unmarshal(raw, &batch); err != nil {
				t.Fatalf("decode batch frame: %v", err)
			}
			out.batchFrames++
			out.events = append(out.events, batch.Events...)
		}
	}
}

func TestServer_ReplayMissedEvents(t *testing.T) {
	f := newServerFixture(t)

	// Emit some events BEFORE any client connects. They fill the ring
	// but never go to a live subscriber.
	for i := 0; i < 3; i++ {
		f.bus.Emit("ch1", i)
	}

	conn := f.dial(t)

	// Ask for everything since seq 0: three events, in order, followed
	// by the completion marker.
	got := requestReplay(t, conn, map[string]uint64{"ch1": 0})
	if len(got.events) != 3 {
		t.Fatalf("replayed %d events, want 3", len(got.events))
	}
	for i, evt := range got.events {
		if evt.Channel != "ch1" {
			t.Fatalf("event %d wrong channel: %s", i, evt.Channel)
		}
		if evt.Seq != uint64(i+1) {
			t.Fatalf("event %d wrong seq: %d", i, evt.Seq)
		}
		if evt.Gap {
			t.Fatalf("event %d unexpectedly gap-flagged", i)
		}
	}
}

// A reconnect during heavy streaming is the worst case for per-event
// frames: the ring can hold DefaultRingCapacity entries. Replay ships
// them through the same batch envelope the live pump uses, chunked at
// DefaultCoalesceMaxEvents, preserving order.
func TestServer_ReplayChunksIntoBatchFrames(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.EventBus = NewEventBus(4 * DefaultCoalesceMaxEvents)
	})

	const total = 2*DefaultCoalesceMaxEvents + 7
	for i := 0; i < total; i++ {
		if _, err := f.bus.Emit("ch1", i); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	conn := f.dial(t)
	got := requestReplay(t, conn, map[string]uint64{"ch1": 0})

	if len(got.events) != total {
		t.Fatalf("replayed %d events, want %d", len(got.events), total)
	}
	// Two full chunks plus the trailing partial — and nothing shipped
	// as a standalone event frame, which is what the pre-batching loop
	// would have produced `total` of.
	if got.batchFrames != 3 || got.eventFrames != 0 {
		t.Fatalf("frames = %d batch / %d event, want 3 batch / 0 event",
			got.batchFrames, got.eventFrames)
	}
	for i, evt := range got.events {
		if evt.Channel != "ch1" {
			t.Fatalf("event %d wrong channel: %s", i, evt.Channel)
		}
		if evt.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d (order must survive batching)",
				i, evt.Seq, i+1)
		}
		var payload int
		if err := json.Unmarshal(evt.Data, &payload); err != nil {
			t.Fatalf("event %d payload decode: %v", i, err)
		}
		if payload != i {
			t.Fatalf("event %d payload = %d, want %d", i, payload, i)
		}
	}
}

// A replay whose cursor sits ABOVE the server's head means the client's
// sequence space is not ours — a restarted backend re-seeds every
// channel from 1. Silently replaying nothing would leave the client
// dropping every live event below its stale cursor forever, so the
// server answers with the gap marker that resyncs it.
func TestServer_ReplayCursorAboveHeadGetsGapMarker(t *testing.T) {
	f := newServerFixture(t)
	for i := 0; i < 3; i++ {
		f.bus.Emit("ch1", i)
	}
	conn := f.dial(t)

	got := requestReplay(t, conn, map[string]uint64{"ch1": 9999})
	if len(got.events) != 1 {
		t.Fatalf("replayed %d frames, want exactly one gap marker: %+v", len(got.events), got.events)
	}
	if !got.events[0].Gap {
		t.Fatalf("cursor above head must produce a gap marker, got %+v", got.events[0])
	}
	if got.events[0].Seq != 3 {
		t.Fatalf("gap marker seq = %d, want the server's head seq 3", got.events[0].Seq)
	}
}

// Same invalid cursor on a latest-only channel: the eviction-side gap
// is deliberately answered with the newest frame instead of a marker,
// but that frame's seq sits BELOW the stale cursor and the client would
// discard it as a duplicate. The marker has to win here.
func TestServer_ReplayCursorAboveHeadGapsLatestOnlyChannel(t *testing.T) {
	f := newServerFixture(t)
	const channel = "system:stats"
	if channelRetention(channel) != RetentionLatestOnly {
		t.Fatalf("%s is no longer a latest-only channel; pick another fixture", channel)
	}
	for i := 0; i < 3; i++ {
		f.bus.Emit(eventchan.Channel(channel), i)
	}
	conn := f.dial(t)

	got := requestReplay(t, conn, map[string]uint64{channel: 9999})
	if len(got.events) != 1 || !got.events[0].Gap {
		t.Fatalf("cursor above head on a latest-only channel must gap, got %+v", got.events)
	}

	// The eviction-side gap on the same channel keeps its newest-frame
	// answer — the two causes must not collapse into one behavior.
	evicted := requestReplay(t, conn, map[string]uint64{channel: 1})
	if len(evicted.events) != 1 {
		t.Fatalf("in-window replay = %+v, want the newest frame", evicted.events)
	}
	if evicted.events[0].Gap {
		t.Fatalf("eviction-side replay on a latest-only channel must not gap: %+v", evicted.events[0])
	}
	if evicted.events[0].Seq != 3 {
		t.Fatalf("newest frame seq = %d, want 3", evicted.events[0].Seq)
	}
}

// A client that vanishes mid-replay must tear the connection down, not
// wedge the handler: every write goes through writeRaw's bound and the
// read loop owns teardown. The connection's subscriber going away is
// the observable proof — it is released by runConnHandler's defer.
func TestServer_ClientDropsDuringReplay(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.EventBus = NewEventBus(DefaultRingCapacity)
	})
	// Payloads big enough that the replay can't complete inside one
	// write, so the close lands mid-stream.
	payload := strings.Repeat("x", 8*1024)
	for i := 0; i < DefaultRingCapacity; i++ {
		if _, err := f.bus.Emit("ch1", payload); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	url := "ws://" + f.srv.Addr() + "/ws?token=test-token"
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	buf, err := json.Marshal(ClientFrame{
		Type:             frameTypeReplay,
		LastSeqByChannel: map[string]uint64{"ch1": 0},
	})
	if err != nil {
		t.Fatalf("marshal replay: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	// Rip the socket away without reading the replay out.
	_ = conn.CloseNow()

	deadline := time.Now().Add(5 * time.Second)
	for f.bus.SubscriberCount() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("connection handler still attached after the client dropped mid-replay")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServer_ReplayGapMarker(t *testing.T) {
	d := NewDispatcher()
	app := &fakeApp{}
	d.Register(app, RegisterOptions{Package: "main", TypeName: "App"})
	bus := NewEventBus(2) // tiny ring forces eviction
	srv, err := New(Config{Dispatcher: d, EventBus: bus, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})

	// Emit 5 events into a 2-cap ring — oldest 3 evicted.
	for i := 0; i < 5; i++ {
		bus.Emit("ch1", i)
	}

	url := "ws://" + srv.Addr() + "/ws?token=test-token"
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	frame := ClientFrame{
		Type:             frameTypeReplay,
		LastSeqByChannel: map[string]uint64{"ch1": 0}, // out of window
	}
	buf, _ := json.Marshal(frame)
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatal(err)
	}

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var f ServerFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		if f.Type != frameTypeEvent {
			continue
		}
		if !f.Gap {
			t.Fatalf("expected gap=true marker, got %+v", f)
		}
		if f.Seq != 5 {
			t.Fatalf("gap should carry head seq=5, got %d", f.Seq)
		}
		return
	}
}

// Replay frames with too many channels must be rejected at the wire
// before reaching the bus, capping memory exposure on adversarial
// input.
func TestServer_ReplayMapTooLarge(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	huge := make(map[string]uint64, MaxReplayChannels+1)
	for i := 0; i <= MaxReplayChannels; i++ {
		huge[fmt.Sprintf("ch%d", i)] = 0
	}
	frame := ClientFrame{
		Type:             frameTypeReplay,
		LastSeqByChannel: huge,
	}
	buf, _ := json.Marshal(frame)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatalf("ws write: %v", err)
	}

	var resp ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != ErrCodeBadParams {
		t.Fatalf("expected bad_params error frame, got %+v", resp)
	}
}

func TestServer_ConcurrentRPCs(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	// Drive 50 concurrent Add calls on the same connection. Each
	// expects a unique (id, result) — we serialize writes via the
	// websocket library but issue from a worker pool to exercise the
	// per-RPC goroutine in handleRPC.
	const n = 50
	type result struct {
		id   string
		val  int
		fail bool
	}
	requests := make(chan int, n)
	results := make(chan result, n)
	var wg sync.WaitGroup

	// Single reader goroutine because the WS lib doesn't support
	// concurrent reads. It collects responses and routes them by id.
	pending := make(map[string]chan ServerFrame)
	var pendingMu sync.Mutex

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var resp ServerFrame
			if err := json.Unmarshal(raw, &resp); err != nil {
				continue
			}
			if resp.Type != frameTypeRPC {
				continue
			}
			pendingMu.Lock()
			ch, ok := pending[resp.ID]
			pendingMu.Unlock()
			if ok {
				ch <- resp
			}
		}
	}()

	// 4 workers driving the request channel.
	var writeMu sync.Mutex
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for x := range requests {
				id := fmt.Sprintf("req-%d", x)
				ch := make(chan ServerFrame, 1)
				pendingMu.Lock()
				pending[id] = ch
				pendingMu.Unlock()

				params := []json.RawMessage{
					json.RawMessage(fmt.Sprintf("%d", x)),
					json.RawMessage(fmt.Sprintf("%d", x)),
				}
				frame := ClientFrame{
					Type:     frameTypeRPC,
					ID:       id,
					MethodID: fnvHash("main.App.Add"),
					Params:   params,
				}
				buf, _ := json.Marshal(frame)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				writeMu.Lock()
				err := conn.Write(ctx, websocket.MessageText, buf)
				writeMu.Unlock()
				cancel()
				if err != nil {
					results <- result{id: id, fail: true}
					continue
				}

				select {
				case resp := <-ch:
					if resp.Error != nil {
						results <- result{id: id, fail: true}
						continue
					}
					var v int
					json.Unmarshal(resp.Result, &v)
					results <- result{id: id, val: v}
				case <-time.After(2 * time.Second):
					results <- result{id: id, fail: true}
				}
			}
		}()
	}

	for i := 0; i < n; i++ {
		requests <- i
	}
	close(requests)
	wg.Wait()
	close(results)

	got := 0
	for r := range results {
		if r.fail {
			t.Errorf("request %s failed", r.id)
		} else {
			got++
		}
	}
	if got != n {
		t.Fatalf("got %d successes, want %d", got, n)
	}
}

func TestServer_BadFrameReturnsError(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	var resp ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error frame for malformed input")
	}
	if resp.Error.Code != ErrCodeBadParams {
		t.Fatalf("expected bad_params, got %s", resp.Error.Code)
	}
}

func TestServer_MethodNotFound(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	resp := f.rpc(t, conn, 0, "DoesNotExist")
	if resp.Error == nil {
		t.Fatalf("expected error frame")
	}
	if resp.Error.Code != ErrCodeMethodNotFound {
		t.Fatalf("expected method_not_found, got %s", resp.Error.Code)
	}
}

// Server.Start is one-shot — a second call must error rather than
// installing a parallel listener. Without this, double-Start leaked
// the first listener's goroutine and Shutdown couldn't clean it up.
func TestServer_StartTwiceIsError(t *testing.T) {
	d := NewDispatcher()
	app := &fakeApp{}
	d.Register(app, RegisterOptions{Package: "main", TypeName: "App"})
	bus := NewEventBus(10)
	srv, err := New(Config{Dispatcher: d, EventBus: bus, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})

	if err := srv.Start(); err == nil {
		t.Fatalf("second Start should error")
	}
	// First listener still serving.
	if !strings.Contains(srv.Addr(), ":") {
		t.Fatalf("Addr corrupted after second Start: %q", srv.Addr())
	}
}

// TestServer_AppURL_PreStartIsEmpty pins the documented contract on
// the pre-Start path: AppURL returns the empty string so callers can
// detect the not-ready state. main.go fatals on empty AppURL — that
// path stays meaningful only if AppURL really returns "" before Start
// rather than emitting a port-less fallback (which earlier code did,
// hitting port 80 and confusing users).
func TestServer_AppURL_PreStartIsEmpty(t *testing.T) {
	d := NewDispatcher()
	bus := NewEventBus(10)
	srv, err := New(Config{Dispatcher: d, EventBus: bus, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.AppURL(); got != "" {
		t.Fatalf("AppURL pre-Start = %q, want empty (so callers can fatal on missing URL)", got)
	}
}

// TestServer_AppURL_PostStartIncludesPort guarantees that the URL the
// webview actually loads always carries the resolved port. The caller
// (main.go) hands this to Wails verbatim; a port-less URL would hit
// port 80, which is unrelated to our listener.
func TestServer_AppURL_PostStartIncludesPort(t *testing.T) {
	f := newServerFixture(t)
	url := f.srv.AppURL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("AppURL %q does not start with http://127.0.0.1:<port>", url)
	}
	// The actual addr's port must appear in the URL.
	_, port, err := net.SplitHostPort(f.srv.Addr())
	if err != nil {
		t.Fatalf("SplitHostPort %q: %v", f.srv.Addr(), err)
	}
	if !strings.Contains(url, ":"+port+"/") {
		t.Fatalf("AppURL %q does not embed live port %q", url, port)
	}
	// Token query param must round-trip.
	if !strings.HasSuffix(url, "?t=test-token") {
		t.Fatalf("AppURL %q missing token query param", url)
	}
}

// TestRebind_NewAddrAccepts verifies that after Rebind, new WS
// connections route to the new listener. We rebind to a fresh ephemeral
// loopback port and confirm an RPC over the new addr succeeds.
func TestRebind_NewAddrAccepts(t *testing.T) {
	f := newServerFixture(t)
	originalAddr := f.srv.Addr()

	if err := f.srv.Rebind("127.0.0.1:0", nil); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if f.srv.Addr() == originalAddr {
		t.Fatalf("Addr did not change after rebind: %q", f.srv.Addr())
	}

	url := "ws://" + f.srv.Addr() + "/ws?token=test-token"
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial new addr: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	resp := f.rpc(t, conn, 0, "Greet", "rebind")
	if resp.Error != nil {
		t.Fatalf("rpc on new addr error: %+v", resp.Error)
	}
	if string(resp.Result) != `"hello, rebind"` {
		t.Fatalf("unexpected result on new addr: %s", string(resp.Result))
	}
}

// TestRebind_OldAddrStopsAccepting verifies that after Rebind, the
// previous listener stops accepting new connections. The old listener's
// http.Server is gracefully shut down and the kernel surrenders the
// port (or, at minimum, refuses to upgrade).
func TestRebind_OldAddrStopsAccepting(t *testing.T) {
	f := newServerFixture(t)
	oldAddr := f.srv.Addr()

	if err := f.srv.Rebind("127.0.0.1:0", nil); err != nil {
		t.Fatalf("rebind: %v", err)
	}

	// Wait for the old listener's serve goroutine to wind down. The
	// retired http.Server.Shutdown is async — give it up to 2s to
	// release the port. Looping with short sleeps keeps the test fast
	// when shutdown is quick.
	ok := waitFor(func() bool {
		dialCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		// We dial bare TCP rather than a full WS handshake — even if
		// the kernel hasn't fully released the port, a conn that
		// closes immediately on accept is enough to prove the old
		// http.Server stopped serving requests.
		conn, dialErr := (&net.Dialer{}).DialContext(dialCtx, "tcp", oldAddr)
		if dialErr != nil {
			return true
		}
		// If we did connect, attempt a WS upgrade — if that fails,
		// new accepts on the old port are no longer being routed
		// through our handler stack.
		_ = conn.Close()
		wsCtx, wsCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer wsCancel()
		c, _, wsErr := websocket.Dial(wsCtx, "ws://"+oldAddr+"/ws?token=test-token", nil)
		if wsErr != nil {
			return true
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
		return false
	}, 2*time.Second)
	if !ok {
		t.Fatalf("old addr %q still accepting new WS connections after rebind", oldAddr)
	}
}

// TestRebind_ExistingWSContinues verifies that a WebSocket connection
// established before Rebind keeps working after the rebind. The
// hijacked TCP connection is owned by the handleWS goroutine; the
// old http.Server.Shutdown does not sever it.
func TestRebind_ExistingWSContinues(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	// Confirm the connection works pre-rebind.
	resp := f.rpc(t, conn, 0, "Greet", "before")
	if resp.Error != nil {
		t.Fatalf("rpc pre-rebind: %+v", resp.Error)
	}

	if err := f.srv.Rebind("127.0.0.1:0", nil); err != nil {
		t.Fatalf("rebind: %v", err)
	}

	// Same connection, same RPC. Must succeed — the WS conn is hijacked
	// off the old listener and lives independently of the listener's
	// http.Server lifecycle.
	resp = f.rpc(t, conn, 0, "Greet", "after")
	if resp.Error != nil {
		t.Fatalf("rpc post-rebind: %+v", resp.Error)
	}
	if string(resp.Result) != `"hello, after"` {
		t.Fatalf("unexpected post-rebind result: %s", string(resp.Result))
	}
}

// TestRebind_ConcurrentWithShutdown verifies that running Rebind and
// Shutdown concurrently doesn't corrupt server state. Either the
// rebind wins and Shutdown cleans up the new listener too, or the
// shutdown wins and Rebind drops with an error.
func TestRebind_ConcurrentWithShutdown(t *testing.T) {
	d := NewDispatcher()
	app := &fakeApp{}
	if _, err := d.Register(app, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	bus := NewEventBus(10)
	srv, err := New(Config{Dispatcher: d, EventBus: bus, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}

	var rebindErr, shutdownErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rebindErr = srv.Rebind("127.0.0.1:0", nil)
	}()
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownErr = srv.Shutdown(ctx)
	}()
	wg.Wait()

	// Whatever the ordering, Shutdown must complete cleanly. Rebind
	// either succeeded (and Shutdown then cleaned up its listener via
	// formerSrvs) or returned the "shut down" sentinel.
	if shutdownErr != nil {
		t.Fatalf("shutdown returned error: %v", shutdownErr)
	}
	if rebindErr != nil && !strings.Contains(rebindErr.Error(), "shut down") {
		t.Fatalf("rebind returned unexpected error: %v", rebindErr)
	}

	// A second Shutdown call must remain a no-op regardless of which
	// branch ran first, proving stopOnce / shutDown stayed coherent.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// Shutdown without Start must not panic and must be a clean no-op.
func TestServer_ShutdownWithoutStart(t *testing.T) {
	d := NewDispatcher()
	bus := NewEventBus(10)
	srv, err := New(Config{Dispatcher: d, EventBus: bus, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown without Start should not error: %v", err)
	}
	// Idempotent.
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown should not error: %v", err)
	}
}

// TestRebind_InvalidAddrLeavesStateIntact pins the state-intact
// contract: a Rebind that fails to acquire its listener (here an
// invalid host) MUST leave the server's observable state exactly as
// it was. Without the contract, callers would have to maintain their
// own rollback bookkeeping on top of a partially-mutated server.
func TestRebind_InvalidAddrLeavesStateIntact(t *testing.T) {
	f := newServerFixture(t)
	preAddr := f.srv.Addr()
	conn := f.dial(t)

	// 999.999.999.999 is not a valid IPv4 — net.Listen rejects it before
	// any state is mutated.
	err := f.srv.Rebind("999.999.999.999:0", nil)
	if err == nil {
		t.Fatalf("expected rebind to fail on invalid addr")
	}

	if got := f.srv.Addr(); got != preAddr {
		t.Fatalf("Addr changed after failed rebind: pre=%q post=%q", preAddr, got)
	}

	// Original WS connection still works — the failed rebind didn't
	// touch the live http.Server / listener pair.
	resp := f.rpc(t, conn, 0, "Greet", "still-alive")
	if resp.Error != nil {
		t.Fatalf("rpc on original conn errored after failed rebind: %+v", resp.Error)
	}
	if string(resp.Result) != `"hello, still-alive"` {
		t.Fatalf("unexpected result on original conn: %s", string(resp.Result))
	}
}

// TestSamePort verifies the port-comparison helper used by
// bindRebindListener to scope the EADDRINUSE recovery. Both addrs must
// parse cleanly with net.SplitHostPort; either malformed yields false
// (better to skip recovery than guess on a partially-parsed addr).
func TestSamePort(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1:8080", "0.0.0.0:8080", true},
		{"[::]:8080", "127.0.0.1:8080", true},
		{"127.0.0.1:8080", "127.0.0.1:9090", false},
		{"127.0.0.1:8080", "", false},
		{"", "127.0.0.1:8080", false},
		{"not-an-addr", "127.0.0.1:8080", false},
	}
	for _, c := range cases {
		if got := samePort(c.a, c.b); got != c.want {
			t.Errorf("samePort(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestRebind_ForeignHolderDifferentPortLeavesStateIntact pins the
// scoping rule on the EADDRINUSE recovery path: when the rebind target
// is on a different port from the live listener, the kernel's
// "address already in use" must be a foreign holder, not Linux's
// self-overlap rule. Closing our own listener wouldn't help — so the
// error propagates directly and the existing listener stays exactly
// as it was.
func TestRebind_ForeignHolderDifferentPortLeavesStateIntact(t *testing.T) {
	f := newServerFixture(t)
	preAddr := f.srv.Addr()
	conn := f.dial(t)

	// Pre-rebind RPC must work.
	resp := f.rpc(t, conn, 0, "Greet", "before")
	if resp.Error != nil {
		t.Fatalf("rpc pre-rebind: %+v", resp.Error)
	}

	// Stage a foreign holder on a fresh ephemeral port (different from
	// f.srv's port). The first net.Listen will return EADDRINUSE
	// because the foreign listener owns it; the recovery path's
	// samePort check sees the port mismatch and propagates without
	// touching our listener.
	foreign, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stage foreign listener: %v", err)
	}
	defer foreign.Close()
	foreignAddr := foreign.Addr().String()

	err = f.srv.Rebind(foreignAddr, nil)
	if err == nil {
		t.Fatalf("expected rebind to fail when target addr is held by a foreign listener")
	}

	// Addr unchanged — recovery never fired.
	if got := f.srv.Addr(); got != preAddr {
		t.Fatalf("Addr changed after failed rebind: pre=%q post=%q", preAddr, got)
	}

	// Pre-rebind hijacked WS connection still works.
	resp = f.rpc(t, conn, 0, "Greet", "still-alive")
	if resp.Error != nil {
		t.Fatalf("rpc on pre-rebind conn after failed rebind: %+v", resp.Error)
	}
	if string(resp.Result) != `"hello, still-alive"` {
		t.Fatalf("unexpected result: %s", string(resp.Result))
	}
}

// TestRebind_SamePortHostFlip pins the Linux self-overlap recovery
// path: rebinding from 127.0.0.1:N to 0.0.0.0:N (the LAN-bind toggle's
// shape) must succeed even though Linux refuses to bind 0.0.0.0:N
// while 127.0.0.1:N is still held by the same process. macOS allows
// the overlap and would have succeeded with the optimistic
// "bind new before retiring old" pattern; Linux requires us to release
// the old listener first. The recovery path closes the old listener,
// retries, and only fails for genuinely-foreign holders. Pre-fix this
// test failed on Linux with "bind: address already in use".
func TestRebind_SamePortHostFlip(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	// Confirm the pre-rebind connection works.
	resp := f.rpc(t, conn, 0, "Greet", "before")
	if resp.Error != nil {
		t.Fatalf("rpc pre-rebind: %+v", resp.Error)
	}

	// Capture the live port and rebind to 0.0.0.0:<same-port>. This is
	// the exact shape SetNetworkSettings uses for the LAN-bind toggle:
	// the port is preserved across the host flip so any URL the user
	// already shared keeps working.
	_, port, err := net.SplitHostPort(f.srv.Addr())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	if err := f.srv.Rebind("0.0.0.0:"+port, nil); err != nil {
		t.Fatalf("rebind 127.0.0.1:%s -> 0.0.0.0:%s: %v", port, port, err)
	}

	// Existing WS connection survives — hijacked goroutines don't depend
	// on the listener.
	resp = f.rpc(t, conn, 0, "Greet", "after")
	if resp.Error != nil {
		t.Fatalf("rpc post-rebind: %+v", resp.Error)
	}
	if string(resp.Result) != `"hello, after"` {
		t.Fatalf("unexpected post-rebind result: %s", string(resp.Result))
	}

	// Flip back to loopback on the same port — the reverse direction
	// also works without leaking listeners.
	if err := f.srv.Rebind("127.0.0.1:"+port, nil); err != nil {
		t.Fatalf("rebind 0.0.0.0:%s -> 127.0.0.1:%s: %v", port, port, err)
	}
	resp = f.rpc(t, conn, 0, "Greet", "after-back")
	if resp.Error != nil {
		t.Fatalf("rpc post-second-rebind: %+v", resp.Error)
	}
}

// TestRebind_SameAddrIsNoOp documents the no-op shortcut: passing the
// current addr with no opts changes returns nil without churning the
// listener. Callers can fire Rebind blindly when settings change
// without comparing addrs themselves.
func TestRebind_SameAddrIsNoOp(t *testing.T) {
	f := newServerFixture(t)
	preAddr := f.srv.Addr()

	// Pre-rebind connection — must continue working untouched after
	// the no-op call.
	conn := f.dial(t)
	resp := f.rpc(t, conn, 0, "Greet", "before")
	if resp.Error != nil {
		t.Fatalf("rpc pre-noop: %+v", resp.Error)
	}

	if err := f.srv.Rebind(preAddr, nil); err != nil {
		t.Fatalf("rebind to same addr should be a no-op: %v", err)
	}

	if got := f.srv.Addr(); got != preAddr {
		t.Fatalf("Addr mutated on no-op rebind: pre=%q post=%q", preAddr, got)
	}

	// Same connection, same RPC.
	resp = f.rpc(t, conn, 0, "Greet", "after")
	if resp.Error != nil {
		t.Fatalf("rpc post-noop: %+v", resp.Error)
	}
}

// TestRebind_OriginPatternsTakeEffect verifies that the WS upgrader
// reads the live (post-rebind) origin allow-list, not the static
// Config value frozen at New(). This is the CSWSH guard for the
// LAN-bind toggle.
func TestRebind_OriginPatternsTakeEffect(t *testing.T) {
	f := newServerFixture(t)

	// Default (loopback) bind has no origin patterns → InsecureSkipVerify
	// → any Origin header is accepted. Tighten the policy to a single
	// allowed origin and prove the upgrader rejects everything else.
	if err := f.srv.Rebind("127.0.0.1:0", &RebindOptions{
		OriginPatterns: []string{"http://allowed.example:*"},
	}); err != nil {
		t.Fatalf("rebind with origin patterns: %v", err)
	}

	wsURL := "ws://" + f.srv.Addr() + "/ws?token=test-token"

	// Browsers send Origin on cross-origin upgrades. The websocket lib
	// uses Host as a default; we explicitly set Origin to a disallowed
	// value to drive the rejection branch.
	disallowedHeader := http.Header{}
	disallowedHeader.Set("Origin", "http://attacker.example")
	_, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: disallowedHeader,
	})
	if err == nil {
		t.Fatalf("upgrade with disallowed Origin should have been rejected")
	}

	// Sanity check: an allowed Origin still goes through.
	allowedHeader := http.Header{}
	allowedHeader.Set("Origin", "http://allowed.example:8080")
	conn, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: allowedHeader,
	})
	if err != nil {
		t.Fatalf("upgrade with allowed Origin failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
}

// TestServer_BootstrapRejectsRebindHost pins the DNS-rebinding defence
// on the HTTP side. In loopback mode (default), a request whose Host
// header names anything other than a loopback alias gets 404. A
// hostile site resolving attacker.tld to 127.0.0.1 cannot harvest the
// bootstrap token by tricking the user into navigating there.
func TestServer_BootstrapRejectsRebindHost(t *testing.T) {
	f := newServerFixture(t)

	// Forge a request with a non-loopback Host. http.Get sets Host
	// from the URL, so we go through the lower-level NewRequest path
	// to override.
	req, err := http.NewRequest("GET", "http://"+f.srv.Addr()+"/bootstrap.json?t=test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "attacker.tld"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for rebind Host, got %d", resp.StatusCode)
	}
}

// TestServer_BootstrapAcceptsLoopbackHosts walks the small set of
// loopback host names the rebind defence whitelists. All three
// canonical forms (127.0.0.1, localhost, [::1]) must reach the
// handler — the guard is for non-loopback Hosts, not all-Hosts.
func TestServer_BootstrapAcceptsLoopbackHosts(t *testing.T) {
	f := newServerFixture(t)
	cases := []string{
		"127.0.0.1",
		"localhost",
		"[::1]",
	}
	for _, host := range cases {
		t.Run(host, func(t *testing.T) {
			req, err := http.NewRequest("GET", "http://"+f.srv.Addr()+"/bootstrap.json?t=test-token", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = host

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Host %q got %d, want 200", host, resp.StatusCode)
			}
		})
	}
}

// TestServer_BootstrapAcceptsAnyHostInLANMode pins the LAN-bind
// release: when the origin allow-list is non-empty, the loopback Host
// guard is a pass-through. A LAN host hitting the bootstrap by IP must
// still reach the handler.
func TestServer_BootstrapAcceptsAnyHostInLANMode(t *testing.T) {
	f := newServerFixture(t)
	// Tighten origin patterns to enter LAN mode without rebinding —
	// SetOriginPatterns flips the allow-list for the live server.
	f.srv.SetOriginPatterns([]string{"http://10.0.0.5:*"})

	req, err := http.NewRequest("GET", "http://"+f.srv.Addr()+"/bootstrap.json?t=test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Non-loopback Host: under loopback-mode guard this would 404, but
	// LAN mode skips the check. Token still validates so a 200 is the
	// expected outcome.
	req.Host = "10.0.0.5"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LAN mode: Host %q got %d, want 200", req.Host, resp.StatusCode)
	}
}

// TestRebind_FormerSrvsBounded pins the cap on retained retired
// http.Servers. A pathological rebind storm (e.g. a UI bug that toggles
// BindAll repeatedly) must not accumulate http.Server instances
// indefinitely; the slice is capped at MaxRetainedFormerSrvs. We rebind
// past the cap synchronously so the goroutine that drains old entries
// after their graceful Shutdown can't beat us to the assertion.
func TestRebind_FormerSrvsBounded(t *testing.T) {
	f := newServerFixture(t)
	for i := 0; i < MaxRetainedFormerSrvs+2; i++ {
		if err := f.srv.Rebind("127.0.0.1:0", nil); err != nil {
			t.Fatalf("rebind %d: %v", i, err)
		}
	}
	f.srv.mu.Lock()
	got := len(f.srv.formerSrvs)
	f.srv.mu.Unlock()
	if got > MaxRetainedFormerSrvs {
		t.Fatalf("formerSrvs len = %d, want <= %d", got, MaxRetainedFormerSrvs)
	}
}

// TestRebind_FormerSrvsDrainsOnShutdownComplete pins the natural drain
// path: when the deferred graceful shutdown returns, the entry is
// removed from formerSrvs. Real apps spend most of their time in this
// regime — graceful shutdown completes well within 5s, the slice goes
// back to empty, and the cap is only reached on adversarial rebind
// loops.
func TestRebind_FormerSrvsDrainsOnShutdownComplete(t *testing.T) {
	f := newServerFixture(t)

	if err := f.srv.Rebind("127.0.0.1:0", nil); err != nil {
		t.Fatalf("rebind: %v", err)
	}

	// The graceful shutdown the rebind kicked off runs on its own
	// goroutine. Poll until the slice drains; bounded retry so a
	// regression doesn't hang the test.
	ok := waitFor(func() bool {
		f.srv.mu.Lock()
		empty := len(f.srv.formerSrvs) == 0
		f.srv.mu.Unlock()
		return empty
	}, 6*time.Second)
	if !ok {
		f.srv.mu.Lock()
		got := len(f.srv.formerSrvs)
		f.srv.mu.Unlock()
		t.Fatalf("formerSrvs did not drain after graceful shutdown: len=%d", got)
	}
}

// TestServer_SetOriginPatterns_LiveRotation pins the helper that
// updates the allow-list without rebinding. Callers (e.g. a future
// "trusted origins" UI) can flip the policy mid-session.
func TestServer_SetOriginPatterns_LiveRotation(t *testing.T) {
	f := newServerFixture(t)

	// Tighten the policy without rebinding.
	f.srv.SetOriginPatterns([]string{"http://only.example:*"})
	wsURL := "ws://" + f.srv.Addr() + "/ws?token=test-token"
	hdr := http.Header{}
	hdr.Set("Origin", "http://attacker.example")
	if _, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: hdr,
	}); err == nil {
		t.Fatalf("upgrade should fail under tightened policy")
	}

	// Loosen back to "any origin" by clearing.
	f.srv.SetOriginPatterns(nil)
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("upgrade should succeed under cleared policy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
}

// TestServer_LocalOnlyMethodEnforcement_LoopbackPathAllowed proves the
// wire-level dispatch routes a LocalOnlyMethods call from a real
// loopback peer through to the receiver. The serverFixture binds on
// 127.0.0.1, so any connection it accepts is by definition loopback —
// dialling it via websocket.Dial drives the same RemoteAddr capture
// path production uses.
func TestServer_LocalOnlyMethodEnforcement_LoopbackPathAllowed(t *testing.T) {
	d := NewDispatcher()
	app := &privilegedApp{}
	if _, err := d.Register(app, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	bus := NewEventBus(20)
	srv, err := New(Config{Dispatcher: d, EventBus: bus, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})

	conn, _, err := websocket.Dial(context.Background(), "ws://"+srv.Addr()+"/ws?token=test-token", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	// OpenTerminal is in LocalOnlyMethods. From a 127.0.0.1 peer the
	// dispatcher must allow it (the embedded webview path is the
	// canonical user).
	frame := ClientFrame{
		Type:     frameTypeRPC,
		ID:       "loopback-1",
		MethodID: fnvHash("main.App.OpenTerminal"),
	}
	buf, _ := json.Marshal(frame)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var resp ServerFrame
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Type != frameTypeRPC || resp.ID != "loopback-1" {
			continue
		}
		if resp.Error != nil {
			t.Fatalf("loopback peer refused privileged method: %+v", resp.Error)
		}
		return
	}
}

// TestRemoteAddrIsLoopback walks the small set of forms r.RemoteAddr
// can take so the LocalOnly enforcement flag stays correct across
// IPv4, IPv6, and the malformed cases httptest sometimes produces.
func TestRemoteAddrIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:54321", true},
		{"127.0.0.1:0", true},
		{"[::1]:54321", true},
		// IPv4-mapped IPv6 loopback — IsLoopback recognises it.
		{"[::ffff:127.0.0.1]:1234", true},
		{"10.0.0.5:54321", false},
		{"192.168.1.5:8080", false},
		{"[fe80::1234]:1234", false},
		{"", false},
		// Malformed: expect false (fail closed) so a synthetic test
		// request can't accidentally bypass LocalOnly enforcement.
		{"not an address", false},
	}
	for _, c := range cases {
		if got := remoteAddrIsLoopback(c.addr); got != c.want {
			t.Errorf("remoteAddrIsLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func readAllAndClose(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func TestServer_CrossOriginIsolationHeaders(t *testing.T) {
	d := NewDispatcher()
	srv, err := New(Config{
		Dispatcher:         d,
		EventBus:           NewEventBus(20),
		Token:              "test-token",
		CrossOriginIsolate: true,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(ctx)
	})

	for _, path := range []string{"/"} {
		resp, err := http.Get("http://" + srv.Addr() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = readAllAndClose(resp)
		for header, want := range map[string]string{
			"Cross-Origin-Opener-Policy":   "same-origin",
			"Cross-Origin-Embedder-Policy": "require-corp",
			"Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := resp.Header.Get(header); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, header, got, want)
			}
		}
	}
}

func TestServer_CrossOriginIsolationOffByDefault(t *testing.T) {
	f := newServerFixture(t)
	resp, err := http.Get("http://" + f.srv.Addr() + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_, _ = readAllAndClose(resp)
	for _, header := range []string{
		"Cross-Origin-Opener-Policy",
		"Cross-Origin-Embedder-Policy",
		"Cross-Origin-Resource-Policy",
	} {
		if got := resp.Header.Get(header); got != "" {
			t.Errorf("GET /: %s = %q, want unset (isolation is diagnostic opt-in)", header, got)
		}
	}
}

func TestWithAssetHeadersCachePolicy(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := withAssetHeaders(inner)
	cases := []struct {
		name       string
		path       string
		remoteAddr string
		want       string
	}{
		// Loopback peers are the embedded webview: it never renavigates,
		// so caching can't pay off but WOULD pin decoded script text in
		// the renderer's in-memory HTTP cache.
		{"loopback asset v4", "/assets/index-abc.js", "127.0.0.1:54321", "no-store"},
		{"loopback asset v6", "/assets/index-abc.js", "[::1]:54321", "no-store"},
		// Remote clients reload across sessions; hashed assets are
		// content-addressed forever.
		{"remote asset", "/assets/index-abc.js", "192.168.1.50:54321", "public, max-age=31536000, immutable"},
		// The SPA shell must never be shadowed by a stale copy.
		{"shell root", "/", "127.0.0.1:54321", "no-cache, must-revalidate"},
		{"shell index", "/index.html", "192.168.1.50:54321", "no-cache, must-revalidate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tc.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.RemoteAddr = tc.remoteAddr
			rec := newHeaderRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want %q", got, tc.want)
			}
		})
	}
}

// headerRecorder is the minimal ResponseWriter these header tests need;
// the full httptest.ResponseRecorder would work too but the package
// currently doesn't import net/http/httptest.
type headerRecorder struct {
	h http.Header
}

func newHeaderRecorder() *headerRecorder              { return &headerRecorder{h: make(http.Header)} }
func (r *headerRecorder) Header() http.Header         { return r.h }
func (r *headerRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *headerRecorder) WriteHeader(int)             {}

// TestServer_KeepaliveHeartbeat verifies the per-connection keepalive
// loop delivers client-visible ping frames at its cadence, and that a
// healthy connection survives past a protocol-ping tick (tick 3 —
// coder/websocket answers the ping automatically inside our Read
// loop). The short cadence is fixture-scoped via Config, so parallel
// runs don't interfere.
func TestServer_KeepaliveHeartbeat(t *testing.T) {
	t.Parallel()
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.KeepaliveInterval = 30 * time.Millisecond
	})
	conn := f.dial(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pings := 0
	for pings < 4 {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read after %d pings: %v", pings, err)
		}
		var frame ServerFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if frame.Type == frameTypePing {
			pings++
		}
	}
}

func TestServer_KeepalivePongTimeoutTearsDownConnection(t *testing.T) {
	t.Parallel()
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.KeepaliveInterval = 25 * time.Millisecond
		cfg.KeepalivePongTimeout = 100 * time.Millisecond
	})
	conn := f.dial(t)

	// Don't read: coder/websocket only answers protocol pings from
	// inside Read, so a non-reading client never pongs — the half-open
	// signature from the server's perspective. The server's keepalive
	// must CloseNow the connection after its pong timeout; once it
	// does, our own reads (which first drain the buffered heartbeat
	// frames) terminate with a close error instead of running to the
	// outer deadline. The sleep must comfortably cover first-ping
	// (3 ticks) + pong timeout (~175ms nominal): once we start reading
	// we answer pings again, so the teardown has to have happened
	// before then.
	time.Sleep(600 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // buffered heartbeat frame
		}
		if ctx.Err() != nil {
			t.Fatalf("server never tore down the unresponsive connection: %v", err)
		}
		return // closed by the server — the behavior under test
	}
}

// newPortTestServer builds a minimal server for the bind-path tests.
func newPortTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.Dispatcher = NewDispatcher()
	cfg.EventBus = NewEventBus(10)
	cfg.Token = "t"
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// Without EphemeralPortFallback, an unavailable port is a hard failure:
// an operator who named a port must not silently land somewhere else.
func TestServer_PortInUseFailsWithoutFallback(t *testing.T) {
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer squatter.Close()

	srv := newPortTestServer(t, Config{BindAddr: "127.0.0.1", Port: squatter.Addr().(*net.TCPAddr).Port})
	if err := srv.Start(); err == nil {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
		t.Fatal("Start succeeded on an occupied port without EphemeralPortFallback")
	}
	if srv.Addr() != "" {
		t.Fatalf("failed Start left an address behind: %q", srv.Addr())
	}
}

// With the fallback opted in, an occupied port retries once on an
// ephemeral one and the caller learns where it landed from Addr().
func TestServer_PortInUseFallsBackToEphemeral(t *testing.T) {
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer squatter.Close()
	taken := squatter.Addr().(*net.TCPAddr).Port

	srv := newPortTestServer(t, Config{BindAddr: "127.0.0.1", Port: taken, EphemeralPortFallback: true})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start with fallback: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})

	_, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split %q: %v", srv.Addr(), err)
	}
	got, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	if got == taken || got == 0 {
		t.Fatalf("fallback bound port %d (occupied port was %d)", got, taken)
	}
}

// The fallback is scoped to port-attributable failures. A bind address
// this host does not own must still fail loudly — retrying it on port 0
// would fail identically and only bury the cause.
func TestServer_UnusableBindAddrFailsDespiteFallback(t *testing.T) {
	srv := newPortTestServer(t, Config{BindAddr: "203.0.113.1", Port: 45123, EphemeralPortFallback: true})
	err := srv.Start()
	if err == nil {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
		t.Fatal("Start succeeded on an unassigned bind address")
	}
	if !strings.Contains(err.Error(), "203.0.113.1") {
		t.Fatalf("error should name the bind address, got %v", err)
	}
}

// TestAddrInUseIsNarrowerThanPortUnavailable pins the split between the
// two bind predicates. portUnavailable answers "would port 0 do better?"
// (the ephemeral fallback's question) and therefore includes EACCES;
// addrInUse answers "would releasing our own listener help?" (the rebind
// recovery's question) and therefore must not.
func TestAddrInUseIsNarrowerThanPortUnavailable(t *testing.T) {
	bindErr := func(errno syscall.Errno) error {
		return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
	}
	cases := []struct {
		name            string
		err             error
		wantInUse       bool
		wantUnavailable bool
	}{
		{"address in use", bindErr(syscall.EADDRINUSE), true, true},
		{"permission denied", bindErr(syscall.EACCES), false, true},
		{"address not available", bindErr(syscall.EADDRNOTAVAIL), false, false},
		{"not a syscall error", errors.New("dial tcp: lookup failed"), false, false},
		{"nil", nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := addrInUse(c.err); got != c.wantInUse {
				t.Errorf("addrInUse(%v) = %v, want %v", c.err, got, c.wantInUse)
			}
			if got := portUnavailable(c.err); got != c.wantUnavailable {
				t.Errorf("portUnavailable(%v) = %v, want %v", c.err, got, c.wantUnavailable)
			}
		})
	}
}

// TestBindRebindListener_PermissionErrorKeepsOldListener is the reason
// the two predicates are separate. The rebind recovery cures a bind
// failure by closing the live listener and retrying; a permission /
// reservation refusal is identical afterwards, so a rebind that hits one
// must propagate with the old listener untouched — otherwise the LAN
// toggle destroys a working server for an error it cannot fix.
//
// Both addrs name the same port so samePort() cannot be what stops the
// recovery: the predicate is the only thing under test.
func TestBindRebindListener_PermissionErrorKeepsOldListener(t *testing.T) {
	const privilegedAddr = "127.0.0.1:80"
	probe, probeErr := net.Listen("tcp", privilegedAddr)
	if probeErr == nil {
		_ = probe.Close()
		t.Skip("this host lets an unprivileged process bind :80; no permission-shaped bind error available")
	}
	if !portUnavailable(probeErr) || addrInUse(probeErr) {
		t.Skipf("binding %s did not produce a permission-shaped error: %v", privilegedAddr, probeErr)
	}

	f := newServerFixture(t)
	old, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stage old listener: %v", err)
	}
	defer old.Close()

	listener, err := f.srv.bindRebindListener(privilegedAddr, old, "0.0.0.0:80")
	if err == nil {
		_ = listener.Close()
		t.Fatal("bindRebindListener bound a privileged port")
	}

	// A second Close on a listener this call already closed returns
	// net.ErrClosed; on a live one it returns nil.
	if closeErr := old.Close(); closeErr != nil {
		t.Fatalf("bindRebindListener closed the live listener for an unrecoverable bind error (%v); close reported %v", err, closeErr)
	}
}

func TestServer_BootstrapAnnouncesHarnessMode(t *testing.T) {
	// The SPA keys its harness bridge on this flag, so it must be absent
	// (not false-y-but-present-and-wrong) on an ordinary boot and true on
	// a harness one.
	plain := newServerFixture(t)
	if bootstrapDoc(t, plain).Harness {
		t.Error("an ordinary boot advertised harness mode")
	}

	harness := newServerFixtureWith(t, func(cfg *Config) { cfg.Harness = true })
	if !bootstrapDoc(t, harness).Harness {
		t.Error("a harness boot did not advertise harness mode")
	}
}

func bootstrapDoc(t *testing.T, f *serverFixture) Bootstrap {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://%s/bootstrap.json?t=test-token", f.srv.Addr()))
	if err != nil {
		t.Fatalf("fetch bootstrap: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("bootstrap status: %d", resp.StatusCode)
	}
	var b Bootstrap
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	return b
}
