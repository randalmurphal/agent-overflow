package transport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// waitForSubscriberCount spins until the bus holds `want` subscribers. The
// handler's teardown runs on its own goroutine after the socket closes, so
// the count is the only observable that says it has finished.
func waitForSubscriberCount(t *testing.T, bus *EventBus, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscriber count stayed at %d, want %d", bus.SubscriberCount(), want)
}

// The connection half of presence.go: a `presence` frame reaches the
// subscriber and nothing else. serverFixture binds 127.0.0.1, so every
// connection it accepts is loopback — which is what LocalScreenPresence
// counts.

// TestConnPresenceFrameStatesTheScreen is the live path end to end: the
// frame is applied, and the gate's question is answered from it.
func TestConnPresenceFrameStatesTheScreen(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypePresence, Focused: true, Threads: []string{"thread-A"}})
	barrier(t, conn, "presence-applied")

	focused, visible := f.bus.LocalScreenPresence("thread-A")
	if !focused || !visible {
		t.Fatalf("presence not applied (focused=%t visible=%t)", focused, visible)
	}
	if _, otherVisible := f.bus.LocalScreenPresence("thread-B"); otherVisible {
		t.Error("a thread the frame did not name reads as on screen")
	}
}

// Each frame REPLACES the last. A person who clicks away and closes the pane
// sends one frame saying both, and the screen stops being attended.
func TestConnPresenceFrameReplacesTheLast(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypePresence, Focused: true, Threads: []string{"thread-A"}})
	sendFrame(t, conn, ClientFrame{Type: frameTypePresence})
	barrier(t, conn, "presence-replaced")

	if focused, visible := f.bus.LocalScreenPresence("thread-A"); focused || visible {
		t.Fatalf("presence latched (focused=%t visible=%t)", focused, visible)
	}
}

// An empty thread array is legal and meaningful: a client sitting on its
// settings page is focused with no thread on screen.
func TestConnPresenceFrameAcceptsAnEmptyThreadSet(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypePresence, Focused: true, Threads: []string{}})
	barrier(t, conn, "presence-empty")

	focused, visible := f.bus.LocalScreenPresence("thread-A")
	if !focused {
		t.Error("an empty thread set must not discard the focus bit")
	}
	if visible {
		t.Error("an empty thread set reported a visible thread")
	}
}

// The watch frame's bounds, and the watch frame's refusal shape: bad_params,
// the previous presence left standing, and a connection that is still usable.
func TestConnPresenceFrameRejectsOversizedInput(t *testing.T) {
	tooMany := make([]string, MaxWatchThreads+1)
	for i := range tooMany {
		tooMany[i] = "t"
	}
	for _, tc := range []struct {
		name    string
		threads []string
	}{
		{"tooManyThreads", tooMany},
		{"emptyID", []string{"thread-A", ""}},
		{"oversizedID", []string{strings.Repeat("t", MaxWatchThreadIDBytes+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServerFixture(t)
			conn := f.dial(t)
			sendFrame(t, conn, ClientFrame{Type: frameTypePresence, Focused: true, Threads: []string{"thread-A"}})
			barrier(t, conn, "first-presence")

			sendFrame(t, conn, ClientFrame{Type: frameTypePresence, ID: "bad", Threads: tc.threads})
			var got ServerFrame
			if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Error == nil || got.Error.Code != ErrCodeBadParams {
				t.Fatalf("frame = %#v, want a bad_params error", got)
			}
			// The refused frame changed nothing: a truncated set would claim
			// a thread is off screen when it is not.
			barrier(t, conn, "after-refusal")
			if focused, visible := f.bus.LocalScreenPresence("thread-A"); !focused || !visible {
				t.Fatalf("a refused frame moved the presence (focused=%t visible=%t)", focused, visible)
			}
		})
	}
}

// A presence frame must not touch delivery. This is the tripwire for the
// off-view work-shedding ban: an attended connection and an unattended one
// receive exactly the same frames.
func TestConnPresenceFrameDoesNotNarrowDelivery(t *testing.T) {
	const filtered = "provider:item_event"
	withEntityFilteredChannel(t, filtered)

	f := newServerFixture(t)
	conn := f.dial(t)
	// Focused, and looking at a DIFFERENT thread than the one about to emit.
	sendFrame(t, conn, ClientFrame{Type: frameTypePresence, Focused: true, Threads: []string{"thread-A"}})
	barrier(t, conn, "presence-applied")

	if _, err := f.bus.EmitEntity(filtered, "thread-Z", "delivered"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	var got ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Channel != filtered {
		t.Fatalf("first frame = %#v, want the entity-filtered channel delivered unchanged", got)
	}
}

// The presence goes with the socket, so a page that closed stops being a
// screen without anything having to remember it.
func TestConnPresenceDropsWhenTheConnectionCloses(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypePresence, Focused: true, Threads: []string{"thread-A"}})
	barrier(t, conn, "presence-applied")
	_ = conn.CloseNow()

	waitForSubscriberCount(t, f.bus, 0)
	if focused, visible := f.bus.LocalScreenPresence("thread-A"); focused || visible {
		t.Fatalf("a closed connection still reads as a screen (focused=%t visible=%t)", focused, visible)
	}
}
