package transport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// deltaPayload builds one `provider:item_event` delta exactly as triage
// emits it, so every assertion below is made against the real wire shape
// rather than against this package's decode struct.
func deltaPayload(t *testing.T, threadID, itemID, text string, updatedAt int64) json.RawMessage {
	t.Helper()
	buf, err := json.Marshal(map[string]any{
		"action":    "delta",
		"threadId":  threadID,
		"itemId":    itemID,
		"kind":      "assistant_text",
		"delta":     text,
		"updatedAt": updatedAt,
	})
	if err != nil {
		t.Fatalf("marshal delta: %v", err)
	}
	return buf
}

func metaPayload(t *testing.T, threadID, itemID string) json.RawMessage {
	t.Helper()
	buf, err := json.Marshal(map[string]any{
		"action":   "meta",
		"threadId": threadID,
		"itemId":   itemID,
		"kind":     "assistant_text",
		"meta":     `{"pathRefs":[]}`,
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	return buf
}

// itemEvent wraps a payload as the bus would hand it to the pump.
func itemEvent(seq uint64, threadID string, data json.RawMessage) Event {
	return Event{
		Channel:   string(eventchan.ProviderItemEvent),
		Seq:       seq,
		Data:      data,
		EntityKey: threadID,
	}
}

// decodeDelta reads a merged frame back through the wire shape.
func decodeDelta(t *testing.T, e Event) leaseItemFrame {
	t.Helper()
	var frame leaseItemFrame
	if err := json.Unmarshal(e.Data, &frame); err != nil {
		t.Fatalf("decode merged frame: %v", err)
	}
	return frame
}

// TestLeaseItemFrameMatchesItemStreamEvent is the drift guard for the local
// decode/encode shape. transport must not import triage's store types, so
// the two structs are pinned by their BYTES: a merged delta has to be
// indistinguishable from the one triage would have emitted for the same
// text, and a renamed field on either side fails here rather than shipping
// merged frames the frontend silently ignores.
func TestLeaseItemFrameMatchesItemStreamEvent(t *testing.T) {
	canonical, err := json.Marshal(triage.ItemStreamEvent{
		Action:    "delta",
		ThreadID:  "thread-A",
		ItemID:    "item-1",
		Kind:      "assistant_text",
		Delta:     "hello",
		UpdatedAt: 1234,
	})
	if err != nil {
		t.Fatalf("marshal triage delta: %v", err)
	}
	local, err := json.Marshal(leaseItemFrame{
		Action:    itemStreamActionDelta,
		ThreadID:  "thread-A",
		ItemID:    "item-1",
		Kind:      "assistant_text",
		Delta:     "hello",
		UpdatedAt: 1234,
	})
	if err != nil {
		t.Fatalf("marshal local delta: %v", err)
	}
	if string(canonical) != string(local) {
		t.Fatalf("merged delta shape drifted:\n triage = %s\n lease  = %s", canonical, local)
	}
	// The upsert key path: transport reads the row id out of `item`, which
	// only exists because NewItemStreamUpsert puts it there.
	upsert, err := json.Marshal(triage.NewItemStreamUpsert(store.Item{
		ID: "item-1", ThreadID: "thread-A", Kind: "assistant_text",
	}))
	if err != nil {
		t.Fatalf("marshal triage upsert: %v", err)
	}
	var decoded leaseItemFrame
	if err := json.Unmarshal(upsert, &decoded); err != nil {
		t.Fatalf("decode triage upsert: %v", err)
	}
	if got := decoded.rowKey(); got != (deltaKey{threadID: "thread-A", itemID: "item-1"}) {
		t.Fatalf("upsert row key = %#v, want the thread/item pair", got)
	}
}

// TestDeltaCoalescerMergesPerRow: two rows stream at once and each is merged
// on its own. Text concatenates in arrival order, and the merged frame
// carries the LAST merged frame's seq so the client's cursor clears every
// frame that was merged away.
func TestDeltaCoalescerMergesPerRow(t *testing.T) {
	var out []Event
	c := deltaCoalescer{window: time.Hour, emit: func(e Event) { out = append(out, e) }}

	for _, e := range []Event{
		itemEvent(1, "thread-A", deltaPayload(t, "thread-A", "item-1", "Hel", 10)),
		itemEvent(2, "thread-A", deltaPayload(t, "thread-A", "item-2", "Wor", 11)),
		itemEvent(3, "thread-A", deltaPayload(t, "thread-A", "item-1", "lo", 12)),
		itemEvent(4, "thread-A", deltaPayload(t, "thread-A", "item-2", "ld", 13)),
	} {
		if !c.intercept(e) {
			t.Fatalf("seq %d passed through; every delta must be absorbed", e.Seq)
		}
	}
	if len(out) != 0 {
		t.Fatalf("frames emitted before the window closed: %d", len(out))
	}
	c.flushAll()
	if len(out) != 2 {
		t.Fatalf("merged frames = %d, want one per row", len(out))
	}
	first, second := decodeDelta(t, out[0]), decodeDelta(t, out[1])
	if first.ItemID != "item-1" || first.Delta != "Hello" || out[0].Seq != 3 {
		t.Fatalf("row 1 merged = %+v seq=%d, want item-1 \"Hello\" seq 3", first, out[0].Seq)
	}
	if second.ItemID != "item-2" || second.Delta != "World" || out[1].Seq != 4 {
		t.Fatalf("row 2 merged = %+v seq=%d, want item-2 \"World\" seq 4", second, out[1].Seq)
	}
	if first.UpdatedAt != 12 || second.UpdatedAt != 13 {
		t.Fatalf("updatedAt = %d/%d, want the last merged frame's stamp (12/13)", first.UpdatedAt, second.UpdatedAt)
	}
	// Seq order is the contract: a client drops any frame at or below its
	// per-channel cursor, so a merge that came out low would lose its text.
	if out[0].Seq >= out[1].Seq {
		t.Fatalf("merged seqs %d then %d are not ascending", out[0].Seq, out[1].Seq)
	}
	for i, e := range out {
		if len(e.WireBytes) == 0 {
			t.Fatalf("merged frame %d has no pre-encoded wire bytes", i)
		}
	}
}

// TestDeltaCoalescerFlushesBeforeMeta: a meta lands AFTER the text it
// re-validates. The frontend relies on it (triage item_events.go), and the
// flush is of every pending row, not just this one, so no lower-seq merge is
// ever left behind a higher-seq pass-through.
func TestDeltaCoalescerFlushesBeforeMeta(t *testing.T) {
	var out []Event
	c := deltaCoalescer{window: time.Hour, emit: func(e Event) { out = append(out, e) }}

	c.intercept(itemEvent(1, "thread-A", deltaPayload(t, "thread-A", "item-1", "one", 10)))
	c.intercept(itemEvent(2, "thread-A", deltaPayload(t, "thread-A", "item-2", "two", 11)))
	meta := itemEvent(3, "thread-A", metaPayload(t, "thread-A", "item-2"))
	if c.intercept(meta) {
		t.Fatal("a meta was absorbed; every non-delta action passes through")
	}
	if len(out) != 2 {
		t.Fatalf("pending merges flushed = %d, want both rows ahead of the meta", len(out))
	}
	if decodeDelta(t, out[0]).ItemID != "item-1" || decodeDelta(t, out[1]).ItemID != "item-2" {
		t.Fatalf("flush order = %s,%s, want ascending seq", out[0].Data, out[1].Data)
	}
	for _, e := range out {
		if e.Seq >= meta.Seq {
			t.Fatalf("merge seq %d is not below the pass-through's %d", e.Seq, meta.Seq)
		}
	}
	// And nothing is left holding: the window is disarmed.
	if c.timerC() != nil && c.armed {
		t.Fatal("the window stayed armed after a flush")
	}
}

// TestDeltaCoalescerPassesThroughOtherFrames pins the fail-open half. A
// payload this build cannot key, and a channel it does not coalesce, both go
// out untouched — a shape change costs wire bytes, never a row.
func TestDeltaCoalescerPassesThroughOtherFrames(t *testing.T) {
	var out []Event
	c := deltaCoalescer{window: time.Hour, emit: func(e Event) { out = append(out, e) }}

	for _, tc := range []struct {
		name  string
		event Event
	}{
		{"otherChannel", Event{Channel: "thread:updated", Seq: 1, Data: json.RawMessage(`{"id":"t"}`)}},
		{"undecodable", itemEvent(2, "thread-A", json.RawMessage(`"not an object"`))},
		{"deltaWithoutItemID", itemEvent(3, "thread-A", deltaPayload(t, "thread-A", "", "x", 1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c.intercept(tc.event) {
				t.Fatalf("%s was absorbed; it must pass through", tc.name)
			}
		})
	}
	if len(out) != 0 {
		t.Fatalf("pass-through frames were re-emitted: %d", len(out))
	}
}

// TestDeltaCoalescerCarriesGapForward: `deliver` stamps a loss announcement
// on whichever frame it delivers and then forgets it, so a merge that ate
// that frame has to carry the flag. Dropping it would swallow the only
// resync instruction the client had coming.
func TestDeltaCoalescerCarriesGapForward(t *testing.T) {
	var out []Event
	c := deltaCoalescer{window: time.Hour, emit: func(e Event) { out = append(out, e) }}

	gapped := itemEvent(1, "thread-A", deltaPayload(t, "thread-A", "item-1", "a", 1))
	gapped.Gap = true
	c.intercept(gapped)
	c.intercept(itemEvent(2, "thread-A", deltaPayload(t, "thread-A", "item-1", "b", 2)))
	c.flushAll()

	if len(out) != 1 || !out[0].Gap {
		t.Fatalf("merged frames = %+v, want one carrying gap:true", out)
	}
	if !strings.Contains(string(out[0].WireBytes), `"gap":true`) {
		t.Fatalf("the encoded frame lost the gap flag: %s", out[0].WireBytes)
	}
}

// TestSubscriberBackgroundWithholdsSeeds is the withholding half, at the
// seam it actually runs on: the subscriber's enqueue-time filters, ahead of
// gap accounting. A withheld seed must leave the channel unflagged, and
// resuming must restore it.
func TestSubscriberBackgroundWithholdsSeeds(t *testing.T) {
	bus := NewEventBus(20)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()

	sub.SetBackground(true)
	if _, err := bus.Emit(eventchan.HighlightSeed, map[string]any{"threadId": "thread-A"}); err != nil {
		t.Fatalf("emit seed: %v", err)
	}
	// A channel the lease does not touch, emitted after, proves the seed was
	// withheld rather than merely late: one ordered fanout, one buffer.
	if _, err := bus.Emit(eventchan.ProviderTurnCompleted, map[string]any{"threadId": "thread-A"}); err != nil {
		t.Fatalf("emit turn: %v", err)
	}
	got := receiveEvent(t, sub)
	if got.Channel != string(eventchan.ProviderTurnCompleted) {
		t.Fatalf("first frame = %s, want the seed withheld and the turn delivered", got.Channel)
	}
	if got.Gap {
		t.Fatal("a withheld seed flagged the channel; withholding must precede gap accounting")
	}
	if len(sub.gapped) != 0 {
		t.Fatalf("gapped channels after a withhold: %v", sub.gapped)
	}

	sub.SetBackground(false)
	if _, err := bus.Emit(eventchan.HighlightSeed, map[string]any{"threadId": "thread-A"}); err != nil {
		t.Fatalf("emit seed after resume: %v", err)
	}
	if got := receiveEvent(t, sub); got.Channel != string(eventchan.HighlightSeed) {
		t.Fatalf("after resume the first frame = %s, want the seed delivered", got.Channel)
	}
}

// TestSubscriberWithoutLeaseDeliversSeeds is the compatibility floor for the
// withholding half: a subscriber that never leased anything receives exactly
// what it did before the frame existed.
func TestSubscriberWithoutLeaseDeliversSeeds(t *testing.T) {
	bus := NewEventBus(20)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()

	if _, err := bus.Emit(eventchan.HighlightSeed, map[string]any{"threadId": "thread-A"}); err != nil {
		t.Fatalf("emit seed: %v", err)
	}
	if got := receiveEvent(t, sub); got.Channel != string(eventchan.HighlightSeed) {
		t.Fatalf("first frame = %s, want the seed delivered unfiltered", got.Channel)
	}
}

func receiveEvent(t *testing.T, sub *Subscriber) Event {
	t.Helper()
	select {
	case e := <-sub.Events():
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event delivered")
		return Event{}
	}
}
