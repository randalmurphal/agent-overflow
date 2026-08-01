package transport

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestSpliceBatchFrame_MatchesLegacyEntries pins the spliced batch
// frame against the retired per-batch marshal: the result must be
// valid JSON that parses through batchFrame into the exact same
// channel/seq/data/gap set and order the batchEventEntry re-marshal
// produced. The only tolerated wire difference is the inert
// `"type":"event"` field each spliced entry inherits from its
// pre-encoded envelope, which encoding/json (and every batch consumer)
// ignores on decode.
func TestSpliceBatchFrame_MatchesLegacyEntries(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	var events []Event
	for _, emit := range []struct {
		channel string
		payload any
	}{
		{"provider:item_event", map[string]any{"delta": "chunk <1> & more"}},
		{"thread:update", []int{1, 2, 3}},
		{"provider:item_event", "second on same channel"},
	} {
		evt, err := bus.Emit(emit.channel, emit.payload)
		if err != nil {
			t.Fatalf("emit %s: %v", emit.channel, err)
		}
		events = append(events, evt)
	}

	buf := spliceBatchFrame(events)
	if !json.Valid(buf) {
		t.Fatalf("spliced batch frame is not valid JSON: %s", buf)
	}

	var got batchFrame
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("spliced frame does not parse as batchFrame: %v", err)
	}
	if got.Type != frameTypeBatch {
		t.Fatalf("frame type = %q, want %q", got.Type, frameTypeBatch)
	}

	// Reference: what the legacy json.Marshal(batchFrame{...}) path
	// produced for the same events, parsed back the same way.
	legacyEntries := make([]batchEventEntry, len(events))
	for i, e := range events {
		legacyEntries[i] = batchEventEntry{Channel: e.Channel, Seq: e.Seq, Data: e.Data, Gap: e.Gap}
	}
	legacy, err := json.Marshal(batchFrame{Type: frameTypeBatch, Events: legacyEntries})
	if err != nil {
		t.Fatal(err)
	}
	var want batchFrame
	if err := json.Unmarshal(legacy, &want); err != nil {
		t.Fatal(err)
	}

	if len(got.Events) != len(want.Events) {
		t.Fatalf("spliced frame has %d entries, want %d", len(got.Events), len(want.Events))
	}
	for i := range want.Events {
		g, w := got.Events[i], want.Events[i]
		if g.Channel != w.Channel || g.Seq != w.Seq || g.Gap != w.Gap || !bytes.Equal(g.Data, w.Data) {
			t.Fatalf("entry %d diverged:\n got %+v\nwant %+v", i, g, w)
		}
	}
}

// TestSpliceBatchFrame_FallbackEncodesMissingWireBytes covers the
// defensive leg: an event without pre-encoded WireBytes (impossible on
// the live pump, where Emit always pre-encodes) is envelope-encoded in
// place so the splice never emits a hole or a dangling separator.
func TestSpliceBatchFrame_FallbackEncodesMissingWireBytes(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	pre, err := bus.Emit("ch1", "pre-encoded")
	if err != nil {
		t.Fatal(err)
	}
	bare := Event{Channel: "ch2", Seq: 9, Data: json.RawMessage(`{"raw":true}`)}

	buf := spliceBatchFrame([]Event{bare, pre, bare})
	if !json.Valid(buf) {
		t.Fatalf("spliced frame with fallback entries is not valid JSON: %s", buf)
	}
	var got batchFrame
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 3 {
		t.Fatalf("got %d entries, want 3", len(got.Events))
	}
	for i, want := range []Event{bare, pre, bare} {
		g := got.Events[i]
		if g.Channel != want.Channel || g.Seq != want.Seq || !bytes.Equal(g.Data, want.Data) {
			t.Fatalf("entry %d = %+v, want channel=%s seq=%d data=%s", i, g, want.Channel, want.Seq, want.Data)
		}
	}
}
