package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"agent-overflow/internal/eventchan"
)

// sendRawFrame writes a client frame spelled as JSON. The scope tests need
// it because ClientFrame cannot spell `"scopes":[]` (omitempty), and an
// empty scope set is a different statement from an absent one.
func sendRawFrame(t *testing.T, conn *websocket.Conn, raw string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatalf("write frame %s: %v", raw, err)
	}
}

// watchFrame spells a watch frame naming thread-A. scopes is the raw JSON
// value of the field, or "" to leave the field out.
func watchFrame(id, scopes string) string {
	if scopes == "" {
		return fmt.Sprintf(`{"type":"watch","id":%q,"threads":["thread-A"]}`, id)
	}
	return fmt.Sprintf(`{"type":"watch","id":%q,"threads":["thread-A"],"scopes":%s}`, id, scopes)
}

// emitScopedItem publishes one transcript frame at an address, carrying its
// label so the reader can name what arrived.
func emitScopedItem(t *testing.T, f *serverFixture, frame scopedFrame) {
	t.Helper()
	if _, err := f.bus.EmitScoped(eventchan.ProviderItemEvent, frame.thread, frame.scope, map[string]string{"label": frame.label()}); err != nil {
		t.Fatalf("emit %s: %v", frame.label(), err)
	}
}

// entryLabel reads the label emitScopedItem stamped on an entry.
func entryLabel(t *testing.T, entry batchEventEntry) string {
	t.Helper()
	var payload struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(entry.Data, &payload); err != nil {
		t.Fatalf("decode item payload %s: %v", entry.Data, err)
	}
	return payload.Label
}

// scopedFramesUsed is emitted by every live test below, in this order.
var scopedFramesUsed = []scopedFrame{agent2A, agent1A, rootA, agent1B, rootB}

// emitAndCollect emits scopedFramesUsed then a marker on a channel the
// watch set does not narrow, and returns the labels of the transcript
// frames that arrived ahead of the marker. One ordered connection makes the
// marker a fence.
func emitAndCollect(t *testing.T, f *serverFixture, conn *websocket.Conn) []string {
	t.Helper()
	for _, frame := range scopedFramesUsed {
		emitScopedItem(t, f, frame)
	}
	emitMarker(t, f, "thread-A")
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, entry := range wireEntries(t, readPastHello(t, conn)) {
			switch entry.Channel {
			case string(eventchan.ProviderTurnCompleted):
				return got
			case string(eventchan.ProviderItemEvent):
				got = append(got, entryLabel(t, entry))
			}
		}
	}
	t.Fatalf("the marker never arrived; collected %v", got)
	return nil
}

// expectAccepted round-trips a replay barrier and fails on any error frame
// ahead of it, so an accepted watch frame is observed, not assumed.
func expectAccepted(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	sendFrame(t, conn, ClientFrame{Type: frameTypeReplay, ID: id})
	for {
		var frame ServerFrame
		if err := json.Unmarshal(readPastHello(t, conn), &frame); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if frame.Error != nil {
			t.Fatalf("frame was refused: %+v", frame.Error)
		}
		if frame.Type == frameTypeReplay && frame.ID == id {
			return
		}
	}
}

// expectRefused reads the next frame and requires a bad_params refusal
// answering id.
func expectRefused(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	var got ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != ErrCodeBadParams || got.ID != id {
		t.Fatalf("frame = %#v, want a bad_params refusal of %s", got, id)
	}
}

// TestConnWatchScopesNarrowLiveDelivery is the end-to-end live half: a
// connection watching thread-A receives its root rows always, a subagent's
// rows only when it names that agent's scope, and every scope when it
// states no scope set.
func TestConnWatchScopesNarrowLiveDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes string
		want   []string
	}{
		{"absentStatesNoScopeSet", "", labels(agent2A, agent1A, rootA)},
		{"nullStatesNoScopeSet", "null", labels(agent2A, agent1A, rootA)},
		{"emptyArrayViewsNoAgent", "[]", labels(rootA)},
		{"oneAgent", `[{"threadId":"thread-A","scopeRootId":"agent-1"}]`, labels(agent1A, rootA)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServerFixture(t)
			conn := f.dial(t)
			sendRawFrame(t, conn, watchFrame("watch", tc.scopes))
			expectAccepted(t, conn, "after-watch")
			if got := emitAndCollect(t, f, conn); !slices.Equal(got, tc.want) {
				t.Fatalf("delivered %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConnWatchScopesAreAbsolute: each frame replaces the scope set, so an
// agent view that closes stops its rows and one that opens starts them.
func TestConnWatchScopesAreAbsolute(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	sendRawFrame(t, conn, watchFrame("open-1", `[{"threadId":"thread-A","scopeRootId":"agent-1"}]`))
	sendRawFrame(t, conn, watchFrame("swap-to-2", `[{"threadId":"thread-A","scopeRootId":"agent-2"}]`))
	expectAccepted(t, conn, "after-swap")
	if got, want := emitAndCollect(t, f, conn), labels(agent2A, rootA); !slices.Equal(got, want) {
		t.Fatalf("after swapping agents: delivered %v, want %v", got, want)
	}

	sendRawFrame(t, conn, watchFrame("close-all", `[]`))
	expectAccepted(t, conn, "after-close")
	if got, want := emitAndCollect(t, f, conn), labels(rootA); !slices.Equal(got, want) {
		t.Fatalf("after closing every agent: delivered %v, want %v", got, want)
	}
}

// TestConnWatchScopesRefusalLeavesThePreviousSet pins the bounds and what a
// refusal does: bad_params, the connection stays usable, and the set the
// connection had before is still the one applied. Neither cleared (which
// would stop rows an open surface needs) nor widened.
func TestConnWatchScopesRefusalLeavesThePreviousSet(t *testing.T) {
	scope := func(thread, root string) string {
		return fmt.Sprintf(`{"threadId":%q,"scopeRootId":%q}`, thread, root)
	}
	many := func(n int) string {
		entries := make([]string, n)
		for i := range entries {
			entries[i] = scope("thread-A", fmt.Sprintf("agent-%d", i))
		}
		return "[" + strings.Join(entries, ",") + "]"
	}
	long := strings.Repeat("s", MaxWatchThreadIDBytes+1)
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{"tooManyScopes", watchFrame("bad", many(MaxWatchScopes+1))},
		{"emptyScopeRoot", watchFrame("bad", "["+scope("thread-A", "")+"]")},
		{"emptyScopeThread", watchFrame("bad", "["+scope("", "agent-2")+"]")},
		{"missingScopeRoot", watchFrame("bad", `[{"threadId":"thread-A"}]`)},
		{"oversizedScopeRoot", watchFrame("bad", "["+scope("thread-A", long)+"]")},
		{"oversizedScopeThread", watchFrame("bad", "["+scope(long, "agent-2")+"]")},
		{"badThreadsWithValidScopes", `{"type":"watch","id":"bad","threads":["thread-A",""],"scopes":[` + scope("thread-A", "agent-2") + `]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServerFixture(t)
			conn := f.dial(t)
			sendRawFrame(t, conn, watchFrame("good", "["+scope("thread-A", "agent-1")+"]"))
			expectAccepted(t, conn, "after-good")

			sendRawFrame(t, conn, tc.frame)
			expectRefused(t, conn, "bad")
			expectAccepted(t, conn, "after-refusal")

			if got, want := emitAndCollect(t, f, conn), labels(agent1A, rootA); !slices.Equal(got, want) {
				t.Fatalf("after the refusal: delivered %v, want the previous set's %v", got, want)
			}
		})
	}
}

// TestConnWatchScopesAcceptTheBound: exactly MaxWatchScopes is legal, so
// the bound refuses only what exceeds it.
func TestConnWatchScopesAcceptTheBound(t *testing.T) {
	entries := make([]string, MaxWatchScopes)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"threadId":"thread-A","scopeRootId":"agent-%d"}`, i+1)
	}
	f := newServerFixture(t)
	conn := f.dial(t)
	sendRawFrame(t, conn, watchFrame("full", "["+strings.Join(entries, ",")+"]"))
	expectAccepted(t, conn, "after-full")
	if got, want := emitAndCollect(t, f, conn), labels(agent2A, agent1A, rootA); !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}

// TestConnWatchScopesNarrowReplay: a reconnecting client restates its
// scopes and then replays, written back to back as wsClient does. Replay
// returns only the rows it would have received live, and a withheld entry
// is not a gap.
func TestConnWatchScopesNarrowReplay(t *testing.T) {
	f := newServerFixture(t)
	for _, frame := range scopedFramesUsed {
		emitScopedItem(t, f, frame)
	}

	conn := f.dial(t)
	sendRawFrame(t, conn, watchFrame("restate", `[{"threadId":"thread-A","scopeRootId":"agent-1"}]`))
	replay := requestReplay(t, conn, map[string]uint64{string(eventchan.ProviderItemEvent): 0})

	var got []string
	for _, entry := range replay.events {
		if entry.Gap {
			t.Fatalf("a replay entry announced a gap; withholding is not a loss: %+v", entry)
		}
		got = append(got, entryLabel(t, entry))
	}
	if want := labels(agent1A, rootA); !slices.Equal(got, want) {
		t.Fatalf("replayed %v, want %v", got, want)
	}
}
