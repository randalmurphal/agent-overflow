package transport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// withEntityFilteredChannel adds channel to the derived hot-path set for
// the duration of one test and restores it afterwards.
//
// It exists because both channels the registry filters today are
// RetentionEphemeral (capacity-0 rings), so the REPLAY half of the filter
// has nothing to replay and could not be exercised against a real row. The
// filter still has to be correct the day a retained channel joins the
// column, which is what these tests are for. It touches only the derived
// set, never a policy row, so the channel keeps its real retention.
func withEntityFilteredChannel(t *testing.T, channel string) {
	t.Helper()
	if entityFilteredEventChannels[channel] {
		return
	}
	entityFilteredEventChannels[channel] = true
	t.Cleanup(func() { delete(entityFilteredEventChannels, channel) })
}

// sendFrame writes one client frame and fails the test if the write does.
func sendFrame(t *testing.T, conn *websocket.Conn, frame ClientFrame) {
	t.Helper()
	buf, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal %s frame: %v", frame.Type, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatalf("write %s frame: %v", frame.Type, err)
	}
}

// barrier makes the read loop prove it has processed everything sent
// before it. Frames are handled in order on one goroutine, so a replay
// request with an empty cursor map answers with nothing but its completion
// marker — and that marker's arrival means the watch frame ahead of it has
// already been applied. Without this, every test below would race the
// server's read loop against its own emits.
func barrier(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	sendFrame(t, conn, ClientFrame{Type: frameTypeReplay, ID: id})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("barrier %s never completed", id)
		default:
		}
		var frame ServerFrame
		if err := json.Unmarshal(readPastHello(t, conn), &frame); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if frame.Type == frameTypeReplay && frame.ID == id {
			return
		}
	}
}

// TestConnWatchFrameNarrowsLiveDelivery is the end-to-end live half: a
// connection that named one thread stops receiving an entity-filtered
// channel for every other thread, and keeps receiving every other channel
// for all of them.
func TestConnWatchFrameNarrowsLiveDelivery(t *testing.T) {
	const filtered = "provider:item_event"
	withEntityFilteredChannel(t, filtered)

	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypeWatch, Threads: []string{"thread-A"}})
	barrier(t, conn, "after-watch")

	if _, err := f.bus.EmitEntity(filtered, "thread-B", "foreign"); err != nil {
		t.Fatalf("emit foreign: %v", err)
	}
	if _, err := f.bus.EmitEntity(filtered, "thread-A", "watched"); err != nil {
		t.Fatalf("emit watched: %v", err)
	}

	var got ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != frameTypeEvent || got.Channel != filtered {
		t.Fatalf("first frame after the watch = %#v, want an event on %s", got, filtered)
	}
	if !strings.Contains(string(got.Data), "watched") {
		t.Fatalf("received the foreign thread's frame: %s", got.Data)
	}
}

// TestConnWatchFrameNarrowsReplay: a reconnecting client asked to be
// caught up on its cursor, not on the entities it stopped watching. A
// frame the filter drops produces no event and no gap marker, exactly like
// the origin and channel filters beside it.
func TestConnWatchFrameNarrowsReplay(t *testing.T) {
	const filtered = "provider:item_event"
	withEntityFilteredChannel(t, filtered)

	f := newServerFixture(t)
	if _, err := f.bus.EmitEntity(filtered, "thread-B", "foreign"); err != nil {
		t.Fatalf("emit foreign: %v", err)
	}
	if _, err := f.bus.EmitEntity(filtered, "thread-A", "watched"); err != nil {
		t.Fatalf("emit watched: %v", err)
	}

	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypeWatch, Threads: []string{"thread-A"}})
	replay := requestReplay(t, conn, map[string]uint64{filtered: 0})

	if len(replay.events) != 1 {
		t.Fatalf("replay returned %d entries, want only the watched one: %+v", len(replay.events), replay.events)
	}
	if !strings.Contains(string(replay.events[0].Data), "watched") {
		t.Fatalf("replay returned the foreign thread's frame: %s", replay.events[0].Data)
	}
	if replay.events[0].Gap {
		t.Fatalf("a withheld replay entry announced a gap; withholding is not a loss")
	}
}

// TestConnWatchFrameOrderedBeforeReplayIsWhatTheClientRelieson pins the
// ordering the SPA depends on: the watch frame and the replay request ride
// the same socket, and one read loop handles them in order, so a watch
// written first is applied before the replay it precedes. If that stopped
// holding, a reconnect would replay the whole ring for every thread.
func TestConnWatchFrameOrderedBeforeReplayIsWhatTheClientReliesOn(t *testing.T) {
	const filtered = "provider:item_event"
	withEntityFilteredChannel(t, filtered)

	f := newServerFixture(t)
	for _, thread := range []string{"thread-B", "thread-C", "thread-A"} {
		if _, err := f.bus.EmitEntity(filtered, thread, thread); err != nil {
			t.Fatalf("emit %s: %v", thread, err)
		}
	}

	conn := f.dial(t)
	// Written back to back with no barrier between them — which is
	// precisely the client's sequence in wsClient's open handler.
	sendFrame(t, conn, ClientFrame{Type: frameTypeWatch, Threads: []string{"thread-A"}})
	replay := requestReplay(t, conn, map[string]uint64{filtered: 0})

	if len(replay.events) != 1 {
		t.Fatalf("replay returned %d entries, want 1: %+v", len(replay.events), replay.events)
	}
}

// TestConnWatchFrameEmptySetWatchesNothing: a client with every pane closed
// says so, and the server believes it. Distinct from never having sent one.
func TestConnWatchFrameEmptySetWatchesNothing(t *testing.T) {
	const filtered = "provider:item_event"
	withEntityFilteredChannel(t, filtered)

	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypeWatch, Threads: []string{}})
	barrier(t, conn, "after-empty-watch")

	if _, err := f.bus.EmitEntity(filtered, "thread-A", "withheld"); err != nil {
		t.Fatalf("emit filtered: %v", err)
	}
	if _, err := f.bus.EmitEntity("thread:updated", "thread-A", "delivered"); err != nil {
		t.Fatalf("emit wildcard: %v", err)
	}
	var got ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Channel != "thread:updated" {
		t.Fatalf("first frame = %#v, want the unnarrowed channel", got)
	}
}

// TestConnWatchFrameRejectsOversizedInput pins both bounds. The refusal is
// bad_params — the same shape handleSubscribe answers with — and the
// connection stays usable, because a client that sent one bad frame has
// not lost its socket.
func TestConnWatchFrameRejectsOversizedInput(t *testing.T) {
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
			sendFrame(t, conn, ClientFrame{Type: frameTypeWatch, ID: "bad", Threads: tc.threads})

			var got ServerFrame
			if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Error == nil || got.Error.Code != ErrCodeBadParams {
				t.Fatalf("frame = %#v, want a bad_params error", got)
			}
			// Still usable: the barrier round-trips a later frame.
			barrier(t, conn, "after-refusal")
		})
	}
}

// TestConnWithoutWatchFrameStaysWildcard is the compatibility floor. Every
// Go client in the tree — the WSL launcher's notification client,
// ao-harness, the --connect stub — connects and never sends this frame, and
// each of them must keep receiving exactly what it did before the frame
// existed.
func TestConnWithoutWatchFrameStaysWildcard(t *testing.T) {
	const filtered = "provider:item_event"
	withEntityFilteredChannel(t, filtered)

	f := newServerFixture(t)
	conn := f.dial(t)
	barrier(t, conn, "connected")

	if _, err := f.bus.EmitEntity(filtered, "thread-Z", "delivered"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	var got ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != frameTypeEvent || got.Channel != filtered {
		t.Fatalf("frame = %#v, want the entity-keyed event delivered unfiltered", got)
	}
}
