package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"github.com/coder/websocket"
)

// replyOrderApp emits before it returns, as a bound method that announces
// the row it wrote does.
type replyOrderApp struct {
	bus *EventBus
}

// EmitThenReturn emits n frames, alternating two channels, then returns.
func (a *replyOrderApp) EmitThenReturn(n int) (string, error) {
	for i := range n {
		channel := eventchan.ProviderTurnCompleted
		if i%2 == 1 {
			channel = eventchan.ProviderItemEvent
		}
		if _, err := a.bus.EmitEntity(channel, "thread-1", map[string]any{"threadId": "thread-1", "n": i}); err != nil {
			return "", err
		}
	}
	return "done", nil
}

func newReplyOrderFixture(t *testing.T, window time.Duration) *serverFixture {
	t.Helper()
	return newServerFixtureWith(t, func(cfg *Config) {
		cfg.CoalesceWindow = window
		if _, err := cfg.Dispatcher.Register(&replyOrderApp{bus: cfg.EventBus}, RegisterOptions{Package: "main", TypeName: "ReplyOrder"}); err != nil {
			t.Fatalf("register: %v", err)
		}
	})
}

// sendRPC writes one RPC frame and returns its id.
func sendRPC(t *testing.T, conn *websocket.Conn, method string, params ...any) string {
	t.Helper()
	raw := make([]json.RawMessage, len(params))
	for i, p := range params {
		buf, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal param: %v", err)
		}
		raw[i] = buf
	}
	frame := ClientFrame{Type: frameTypeRPC, ID: fmt.Sprintf("req-%d", time.Now().UnixNano()), Method: method, Params: raw}
	buf, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	return frame.ID
}

// wireItem is one thing read off the socket, in arrival order: an event
// entry (from a plain or batch frame) or the reply.
type wireItem struct {
	reply bool
	entry batchEventEntry
}

// readUntilReply reads until the reply with id arrives, returning every
// event entry seen before it and the reply.
func readUntilReply(t *testing.T, conn *websocket.Conn, id string) []wireItem {
	t.Helper()
	var out []wireItem
	for {
		raw := readPastHello(t, conn)
		var probe ServerFrame
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if probe.Type == frameTypeRPC {
			if probe.ID != id {
				continue
			}
			if probe.Error != nil {
				t.Fatalf("rpc error: %+v", probe.Error)
			}
			return append(out, wireItem{reply: true})
		}
		for _, entry := range wireEntries(t, raw) {
			out = append(out, wireItem{entry: entry})
		}
	}
}

// An event a method emitted before it returned reaches the client before
// the method's reply, even when the batching window has not elapsed.
func TestReplyFollowsEventsEmittedBeforeIt(t *testing.T) {
	f := newReplyOrderFixture(t, time.Hour)
	conn := f.dial(t)

	items := readUntilReply(t, conn, sendRPC(t, conn, "EmitThenReturn", 3))

	if len(items) != 4 || !items[3].reply {
		t.Fatalf("want 3 events then the reply, got %+v", items)
	}
}

// The flush ahead of a reply keeps the events in emission order, across
// channels and within each.
func TestReplyFlushKeepsEventOrder(t *testing.T) {
	f := newReplyOrderFixture(t, time.Hour)
	conn := f.dial(t)

	items := readUntilReply(t, conn, sendRPC(t, conn, "EmitThenReturn", 6))

	if len(items) != 7 {
		t.Fatalf("want 6 events then the reply, got %d items", len(items))
	}
	for i, item := range items[:6] {
		var payload struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(item.entry.Data, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload.N != i {
			t.Fatalf("event %d carried n=%d; order %+v", i, payload.N, items)
		}
	}
}

// Events with no reply behind them still go out as one batch per window.
func TestEventBurstWithoutRPCStillBatches(t *testing.T) {
	f := newReplyOrderFixture(t, 200*time.Millisecond)
	conn := f.dial(t)
	// The upgrade completes before the connection subscribes; an emit in
	// between reaches no subscriber.
	barrier(t, conn, "connected")
	app := &replyOrderApp{bus: f.bus}

	if _, err := app.EmitThenReturn(10); err != nil {
		t.Fatalf("emit: %v", err)
	}

	frames, events := 0, 0
	for events < 10 {
		events += len(wireEntries(t, readPastHello(t, conn)))
		frames++
	}
	if frames != 1 || events != 10 {
		t.Fatalf("want 10 events in 1 frame, got %d in %d", events, frames)
	}
}

// A reply does not flush the deltas a background lease is merging: they are
// held for the lease window, and the reply goes out without them.
func TestReplyLeavesLeaseMergesHeld(t *testing.T) {
	f := newReplyOrderFixture(t, DefaultCoalesceWindow)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypeLease, State: leaseStateBackground})
	barrier(t, conn, "after-lease")
	emitDelta(t, f, "thread-A", "item-1", "Hel", 10)

	items := readUntilReply(t, conn, sendRPC(t, conn, "EmitThenReturn", 0))

	for _, item := range items {
		if delta, ok := asDelta(t, item.entry); !item.reply && ok {
			t.Fatalf("the reply flushed a held delta %+v", delta)
		}
	}
	if got := deltasUntil(t, conn, 1); got[0].text != "Hel" {
		t.Fatalf("held delta = %+v, want \"Hel\"", got)
	}
}
