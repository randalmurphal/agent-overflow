package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"agent-overflow/internal/transport"

	"github.com/coder/websocket"
)

// TestIntegration_AppEmitReachesWSClient pins the Phase C wire
// contract end-to-end: an App.emit call must fan out to a connected WS
// client as a `{type:"event"}` frame with per-channel monotonic seq
// and a raw-payload `data` field (no SeqEnvelope wrap). The integration
// test in internal/transport exercises the dispatcher + bus in
// isolation; this one exercises the App→bus→WS path so a regression
// like SetEventBus failing to wire the bus, or a.emit reaching for a
// stale field, fails here even though the unit tests still pass.
//
// We don't register the App with the dispatcher because the test only
// needs the emit path. A bare RPC dispatcher with no methods is fine —
// the WS handshake completes regardless and the event delivery is what
// matters.
func TestIntegration_AppEmitReachesWSClient(t *testing.T) {
	app := NewApp()

	dispatcher := transport.NewDispatcher()
	bus := transport.NewEventBus(64)
	app.SetEventBus(bus)

	// Bind on an ephemeral loopback port so the test can dial without
	// colliding with concurrent runs.
	srv, err := transport.New(transport.Config{
		Dispatcher: dispatcher,
		EventBus:   bus,
		Token:      "app-emit-integration-token",
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("transport.Server.Start: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})

	host, port, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	wsURL := fmt.Sprintf("ws://%s:%s/ws?token=app-emit-integration-token", host, port)

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	// websocket.Dial returns once the upgrade handshake completes, but
	// the server-side subscription happens after that — runConnHandler
	// calls bus.Subscribe in its body, scheduled separately from the
	// upgrade. If we emit before that runs, the events are dropped on
	// the floor with no subscriber. Wait for the bus to see the live
	// subscription so the assertions are deterministic.
	if !waitForSubscriber(bus, 1, time.Second) {
		t.Fatalf("server never registered WS subscriber")
	}

	// Emit twice on the same channel via App.emit. The first payload
	// MUST land as seq=1, the second as seq=2 — the bus owns the
	// per-channel counter and must be the one assigning these.
	app.emit("test:channel", map[string]any{"hello": "world"})
	app.emit("test:channel", map[string]any{"hello": "again"})

	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()

	first := readNextEventFrame(t, conn, readCtx, "test:channel")
	if first.Seq != 1 {
		t.Fatalf("first event seq = %d, want 1", first.Seq)
	}
	var firstData struct {
		Hello string `json:"hello"`
	}
	if err := json.Unmarshal(first.Data, &firstData); err != nil {
		t.Fatalf("unmarshal first data: %v (raw: %s)", err, string(first.Data))
	}
	if firstData.Hello != "world" {
		t.Fatalf("first data.hello = %q, want \"world\"", firstData.Hello)
	}

	second := readNextEventFrame(t, conn, readCtx, "test:channel")
	if second.Seq != 2 {
		t.Fatalf("second event seq = %d, want 2", second.Seq)
	}
	var secondData struct {
		Hello string `json:"hello"`
	}
	if err := json.Unmarshal(second.Data, &secondData); err != nil {
		t.Fatalf("unmarshal second data: %v", err)
	}
	if secondData.Hello != "again" {
		t.Fatalf("second data.hello = %q, want \"again\"", secondData.Hello)
	}
}

// readNextEventFrame consumes WS messages until it sees an `event`
// frame on the requested channel. Other frames (replay markers,
// keepalives) are ignored so the test stays robust to additions in the
// wire format that aren't this test's concern.
func readNextEventFrame(t *testing.T, conn *websocket.Conn, ctx context.Context, channel string) eventFrame {
	t.Helper()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var frame eventFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("decode frame: %v (raw: %s)", err, string(raw))
		}
		if frame.Type == "event" && frame.Channel == channel {
			return frame
		}
	}
}

// eventFrame mirrors the subset of transport.ServerFrame we care about
// here. We don't import the transport struct because this test is in
// package main and we want the wire shape — the test would still need
// to fail if the JSON tags drifted, and re-using the struct would mask
// that.
type eventFrame struct {
	Type    string          `json:"type"`
	Channel string          `json:"channel"`
	Seq     uint64          `json:"seq"`
	Data    json.RawMessage `json:"data"`
}

// waitForSubscriber polls bus.SubscriberCount until it reaches at least
// `min` or the timeout expires. websocket.Dial returns when the upgrade
// completes, but the server-side runConnHandler — which is what calls
// bus.Subscribe — runs on its own goroutine and may not have attached
// the subscriber yet. Polling via the public accessor avoids reaching
// into transport package internals.
func waitForSubscriber(bus *transport.EventBus, min int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount() >= min {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return bus.SubscriberCount() >= min
}
