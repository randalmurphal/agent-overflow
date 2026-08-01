package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestEventBus_EmitAssignsSeq(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()

	a, _ := bus.Emit("ch1", map[string]int{"x": 1})
	b, _ := bus.Emit("ch1", map[string]int{"x": 2})
	c, _ := bus.Emit("ch2", map[string]int{"y": 1})

	if a.Seq != 1 || b.Seq != 2 {
		t.Fatalf("expected ch1 seq 1,2; got %d,%d", a.Seq, b.Seq)
	}
	if c.Seq != 1 {
		t.Fatalf("ch2 should have its own counter, got %d", c.Seq)
	}
}

func TestEventBus_RingBoundedDropsOldest(t *testing.T) {
	bus := NewEventBus(3)
	defer bus.Close()

	for i := 0; i < 5; i++ {
		bus.Emit("ch1", i)
	}

	// Replay from seq 0 — oldestSeq is now 3, so client at seq 0
	// should receive a gap marker, not partial history.
	out := bus.Replay(map[string]uint64{"ch1": 0})
	if len(out) != 1 {
		t.Fatalf("expected gap marker, got %d events", len(out))
	}
	if !out[0].Gap {
		t.Fatalf("expected gap=true, got %+v", out[0])
	}
	if out[0].Seq != 5 {
		t.Fatalf("gap should carry current head seq=5, got %d", out[0].Seq)
	}
}

// After eviction, an in-window client must still receive the surviving
// entries in order. Strengthens TestEventBus_RingBoundedDropsOldest:
// proves eviction preserves the post-eviction ring contents, not just
// that the gap marker fires.
func TestEventBus_PostEvictionInWindowGetsSurvivors(t *testing.T) {
	bus := NewEventBus(3)
	defer bus.Close()

	for i := 0; i < 5; i++ {
		bus.Emit("ch1", i)
	}
	// After 5 emits into a 3-cap ring, surviving seqs are 3,4,5.
	// A client at lastSeq=3 is in-window — should get 4 and 5.
	out := bus.Replay(map[string]uint64{"ch1": 3})
	if len(out) != 2 {
		t.Fatalf("expected 2 surviving events, got %d (%+v)", len(out), out)
	}
	if out[0].Seq != 4 || out[1].Seq != 5 {
		t.Fatalf("surviving seqs out of order: %+v", out)
	}
	for _, e := range out {
		if e.Gap {
			t.Fatalf("in-window replay should not have gap markers")
		}
	}
}

func TestEventBus_RingCapacityOne(t *testing.T) {
	// Smallest non-pathological size — exercises the index math edge.
	bus := NewEventBus(1)
	defer bus.Close()

	bus.Emit("ch1", "a")
	bus.Emit("ch1", "b")
	bus.Emit("ch1", "c")

	// Only "c" survives (seq=3). Client at seq 0 is way out of window.
	out := bus.Replay(map[string]uint64{"ch1": 0})
	if len(out) != 1 || !out[0].Gap || out[0].Seq != 3 {
		t.Fatalf("expected gap marker with seq=3, got %+v", out)
	}

	// Client at seq 2 catches the surviving entry.
	out = bus.Replay(map[string]uint64{"ch1": 2})
	if len(out) != 1 || out[0].Gap || out[0].Seq != 3 {
		t.Fatalf("expected single non-gap entry seq=3, got %+v", out)
	}
}

func TestEventBus_Replay_InWindow(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	for i := 1; i <= 5; i++ {
		bus.Emit("ch1", i)
	}
	out := bus.Replay(map[string]uint64{"ch1": 2})
	if len(out) != 3 {
		t.Fatalf("expected 3 missed events (3,4,5), got %d", len(out))
	}
	for i, e := range out {
		if e.Gap {
			t.Fatalf("in-window replay should not have gap markers: %+v", e)
		}
		if e.Seq != uint64(3+i) {
			t.Fatalf("expected seq %d, got %d", 3+i, e.Seq)
		}
	}
}

func TestEventBus_Replay_AtHeadReturnsNothing(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	for i := 1; i <= 5; i++ {
		bus.Emit("ch1", i)
	}
	out := bus.Replay(map[string]uint64{"ch1": 5})
	if len(out) != 0 {
		t.Fatalf("expected no replay when client at head, got %d events", len(out))
	}
}

func TestEventBus_Replay_FreshClientGetsAll(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	for i := 1; i <= 5; i++ {
		bus.Emit("ch1", i)
	}
	// lastSeq=0 means "I haven't seen anything yet" — within ring,
	// expect every event back.
	out := bus.Replay(map[string]uint64{"ch1": 0})
	if len(out) != 5 {
		t.Fatalf("expected all 5 events, got %d", len(out))
	}
}

func TestEventBus_Replay_UnknownChannelSkipped(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	bus.Emit("ch1", 1)
	out := bus.Replay(map[string]uint64{"never-emitted": 0})
	if len(out) != 0 {
		t.Fatalf("unknown channel should produce no events, got %d", len(out))
	}
}

// Replay with a mix of known + unknown channels returns only the
// known channels' events; unknown channels silently no-op.
func TestEventBus_Replay_MixedKnownUnknown(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	bus.Emit("ch1", "alpha")
	out := bus.Replay(map[string]uint64{
		"ch1":     0,
		"unknown": 5,
	})
	if len(out) != 1 || out[0].Channel != "ch1" {
		t.Fatalf("expected single ch1 event, got %+v", out)
	}
}

// Empty LastSeqByChannel short-circuits — production fresh-connect
// flow sends {} and expects no replay (live deliveries take over).
func TestEventBus_Replay_EmptyMapIsNil(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()
	bus.Emit("ch1", "a")
	out := bus.Replay(map[string]uint64{})
	if len(out) != 0 {
		t.Fatalf("expected nil/empty replay for empty input, got %d", len(out))
	}
}

func TestEventBus_Subscribe_LiveDelivery(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	sub := bus.Subscribe()
	defer sub.Close()

	bus.Emit("ch1", "a")
	bus.Emit("ch1", "b")

	got := drainEvents(t, sub, 2, 200*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("expected 2 live events, got %d", len(got))
	}
	if got[0].Channel != "ch1" || got[1].Channel != "ch1" {
		t.Fatalf("channels lost: %+v", got)
	}
}

// Subscriber.Done() must close on bus.Close() so consumers in
// pumpEvents-style select loops exit promptly.
func TestEventBus_CloseSignalsSubscribers(t *testing.T) {
	bus := NewEventBus(10)
	sub := bus.Subscribe()

	bus.Close()

	select {
	case <-sub.Done():
		// expected
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("Subscriber.Done() not closed after bus.Close()")
	}
}

func TestEventBus_SubscriberCount(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	if got := bus.SubscriberCount(); got != 0 {
		t.Fatalf("empty bus reports %d subscribers", got)
	}

	a := bus.Subscribe()
	b := bus.Subscribe()
	if got := bus.SubscriberCount(); got != 2 {
		t.Fatalf("expected 2 subscribers, got %d", got)
	}

	a.Close()
	if got := bus.SubscriberCount(); got != 1 {
		t.Fatalf("expected 1 subscriber after Close, got %d", got)
	}
	b.Close()
}

func TestEventBus_ChannelFilteredSubscriber(t *testing.T) {
	bus := NewEventBus(8)
	t.Cleanup(bus.Close)
	sub := bus.Subscribe()
	t.Cleanup(sub.Close)
	sub.SetChannels([]string{"notification:send"})

	if got := bus.ChannelSubscriberCount("notification:send"); got != 1 {
		t.Fatalf("notification subscriber count = %d, want 1", got)
	}
	if got := bus.ChannelSubscriberCount("provider:item_event"); got != 0 {
		t.Fatalf("provider subscriber count = %d, want 0", got)
	}
	if _, err := bus.Emit("provider:item_event", "ignored"); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Emit("notification:send", "wanted"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sub.Events():
		if event.Channel != "notification:send" {
			t.Fatalf("delivered channel = %q, want notification:send", event.Channel)
		}
	case <-time.After(time.Second):
		t.Fatal("filtered subscriber did not receive selected channel")
	}
}

func TestEventBus_Subscribe_AfterClose(t *testing.T) {
	bus := NewEventBus(10)
	bus.Close()
	// Emit after Close is silent no-op.
	if _, err := bus.Emit("ch1", "x"); err != nil {
		t.Fatalf("post-close Emit returned error: %v", err)
	}
}

func TestEventBus_ConcurrentEmit_AllPersist(t *testing.T) {
	bus := NewEventBus(1000)
	defer bus.Close()

	const publishers = 4
	const each = 100
	var wg sync.WaitGroup
	wg.Add(publishers)
	for p := 0; p < publishers; p++ {
		go func(pid int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				bus.Emit("ch1", map[string]int{"p": pid, "i": i})
			}
		}(p)
	}
	wg.Wait()

	out := bus.Replay(map[string]uint64{"ch1": 0})
	if len(out) != publishers*each {
		t.Fatalf("expected %d events, got %d (concurrent publish lost frames)",
			publishers*each, len(out))
	}
	// Sequences must be a contiguous 1..N (no gaps under concurrency).
	for i, e := range out {
		if e.Seq != uint64(i+1) {
			t.Fatalf("expected contiguous seq, got %d at index %d", e.Seq, i)
		}
	}
}

func TestEventBus_DataIsRawJSON(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	type payload struct {
		Title string `json:"title"`
	}
	evt, err := bus.Emit("ch1", payload{Title: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var got payload
	if err := json.Unmarshal(evt.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "hello" {
		t.Fatalf("payload corrupted: %q", got.Title)
	}
}

// TestEventBus_SlowSubscriberDropsButRingPersists pins the asymmetric
// drop contract: a slow subscriber that doesn't drain its delivery
// channel saturates at DefaultSubscriberBuffer, but the per-channel
// ring keeps storing every emit so a Replay request from the same
// subscriber (after it catches up) sees the full sequence.
//
// This is the load-bearing rule that makes wsClient's per-channel
// seq-gap detection work: the bus drops to the slow subscriber, the
// next event the subscriber reads carries a non-contiguous seq, and
// the client re-fetches via list endpoints. Without ring persistence,
// a slow subscriber would lose data permanently — even after catching
// up, no Replay path could recover it.
func TestEventBus_SlowSubscriberDropsButRingPersists(t *testing.T) {
	bus := NewEventBus(0) // default ring capacity (1000)
	defer bus.Close()

	sub := bus.Subscribe()
	defer sub.Close()

	// Emit beyond the subscriber buffer cap. The bus's deliver() drops
	// silently on a full buffer; the ring keeps appending up to the
	// ring capacity.
	const overflow = DefaultSubscriberBuffer + 10
	for i := 0; i < overflow; i++ {
		if _, err := bus.Emit("ch1", i); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	// The subscriber's buffered channel is at most DefaultSubscriberBuffer
	// items — the deliveries past that point dropped silently. Drain
	// what we can and assert we never see more than the cap.
	drained := drainEvents(t, sub, DefaultSubscriberBuffer+1, 200*time.Millisecond)
	if len(drained) > DefaultSubscriberBuffer {
		t.Fatalf("subscriber drained %d events, want <= %d (buffer cap)", len(drained), DefaultSubscriberBuffer)
	}

	// The ring kept every event though — Replay from seq 0 must
	// surface all `overflow` emits as long as ring capacity wasn't
	// exceeded. Default ring capacity (1000) is larger than overflow
	// (1024+10) — wait, let's check. overflow = 1034 > 1000, so the
	// oldest 34 should be evicted and we'll see a gap marker.
	out := bus.Replay(map[string]uint64{"ch1": 0})
	// Expected behavior: with a 1000-cap ring and 1034 emits, the
	// oldest 34 evict and the replay returns a gap marker (seq=1034)
	// instead of partial history. That's the documented contract.
	if len(out) != 1 || !out[0].Gap {
		t.Fatalf("expected gap marker for out-of-window replay, got %d events (gap=%v)", len(out), out[0].Gap)
	}
	if out[0].Seq != uint64(overflow) {
		t.Fatalf("gap should carry head seq=%d, got %d", overflow, out[0].Seq)
	}

	// In-window replay (last surviving event is overflow-cap+1; ask for
	// everything since then-1) should return the surviving entries
	// without a gap marker — proving the ring kept N items even though
	// the subscriber dropped them.
	survivingStart := uint64(overflow - DefaultRingCapacity)
	out = bus.Replay(map[string]uint64{"ch1": survivingStart})
	if len(out) != DefaultRingCapacity {
		t.Fatalf("in-window replay got %d events, want %d", len(out), DefaultRingCapacity)
	}
	for _, e := range out {
		if e.Gap {
			t.Fatalf("in-window replay should not have gap markers")
		}
	}
}

// TestEventBus_PreEncodedWireBytesMatchEnvelope pins the contract that
// the WireBytes pre-encoding produced by Emit is byte-equivalent to the
// ServerFrame{type:"event", channel, seq, data} envelope a per-event
// marshal would produce. The conn pump writes WireBytes verbatim — if
// the encoding diverges (field order, omission, missing wrapper) the
// frontend either rejects the frame or sees ghost shape changes.
//
// We round-trip WireBytes through json.Unmarshal into ServerFrame and
// compare every field against what Emit reported and the original
// payload. Deep-equality on the parsed struct catches both extra and
// missing fields.
func TestEventBus_PreEncodedWireBytesMatchEnvelope(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	type payload struct {
		Title string `json:"title"`
		N     int    `json:"n"`
	}
	in := payload{Title: "hello", N: 7}
	evt, err := bus.Emit("ch1", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(evt.WireBytes) == 0 {
		t.Fatalf("Emit must populate WireBytes; got empty")
	}

	var frame ServerFrame
	if err := json.Unmarshal(evt.WireBytes, &frame); err != nil {
		t.Fatalf("WireBytes is not valid JSON: %v (%s)", err, string(evt.WireBytes))
	}

	if frame.Type != frameTypeEvent {
		t.Errorf("WireBytes Type = %q, want %q", frame.Type, frameTypeEvent)
	}
	if frame.Channel != evt.Channel {
		t.Errorf("WireBytes Channel = %q, want %q", frame.Channel, evt.Channel)
	}
	if frame.Seq != evt.Seq {
		t.Errorf("WireBytes Seq = %d, want %d", frame.Seq, evt.Seq)
	}
	if frame.Gap != evt.Gap {
		t.Errorf("WireBytes Gap = %v, want %v", frame.Gap, evt.Gap)
	}
	// Data must round-trip back to the original payload — proves the
	// envelope didn't double-encode or lose the inner JSON.
	var got payload
	if err := json.Unmarshal(frame.Data, &got); err != nil {
		t.Fatalf("envelope Data not valid JSON: %v", err)
	}
	if got != in {
		t.Errorf("WireBytes Data round-trip = %+v, want %+v", got, in)
	}
}

// TestEventBus_GapMarkerHasWireBytes mirrors the live-pump contract for
// the replay path: a synthetic gap-marker carries WireBytes too, so the
// conn pump can write it without re-marshalling. Without this the gap
// path silently fell back to the marshal slow path on every reconnect.
func TestEventBus_GapMarkerHasWireBytes(t *testing.T) {
	bus := NewEventBus(2)
	defer bus.Close()

	// Overflow the ring so a fresh client at seq=0 sees a gap marker.
	for i := 0; i < 5; i++ {
		bus.Emit("ch1", i)
	}

	out := bus.Replay(map[string]uint64{"ch1": 0})
	if len(out) != 1 || !out[0].Gap {
		t.Fatalf("expected single gap marker, got %d (%+v)", len(out), out)
	}
	gap := out[0]
	if len(gap.WireBytes) == 0 {
		t.Fatalf("gap marker missing WireBytes")
	}
	var frame ServerFrame
	if err := json.Unmarshal(gap.WireBytes, &frame); err != nil {
		t.Fatalf("gap WireBytes invalid JSON: %v", err)
	}
	if !frame.Gap || frame.Type != frameTypeEvent || frame.Channel != "ch1" {
		t.Fatalf("gap WireBytes envelope unexpected: %+v", frame)
	}
}

// TestEventBus_SubscribeMaintainsList pins the maintained-slice
// invariant: subList must mirror the subs map after every join/leave.
// A bug here either delivers events to closed subscribers (ghost in
// list) or silently drops live deliveries to attached subscribers
// (missing from list). Both regressions are silent in the user-facing
// streaming UX, so we assert the invariant explicitly.
func TestEventBus_SubscribeMaintainsList(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	a := bus.Subscribe()
	b := bus.Subscribe()
	c := bus.Subscribe()

	bus.mu.RLock()
	if len(bus.subList) != 3 || len(bus.subs) != 3 {
		bus.mu.RUnlock()
		t.Fatalf("expected 3 subs after 3 Subscribes, got list=%d map=%d", len(bus.subList), len(bus.subs))
	}
	bus.mu.RUnlock()

	b.Close()

	bus.mu.RLock()
	if len(bus.subList) != 2 || len(bus.subs) != 2 {
		bus.mu.RUnlock()
		t.Fatalf("expected 2 subs after Close, got list=%d map=%d", len(bus.subList), len(bus.subs))
	}
	for _, s := range bus.subList {
		if s == b {
			bus.mu.RUnlock()
			t.Fatalf("closed subscriber still in subList")
		}
	}
	bus.mu.RUnlock()

	a.Close()
	c.Close()

	bus.mu.RLock()
	got := len(bus.subList)
	bus.mu.RUnlock()
	if got != 0 {
		t.Fatalf("expected empty subList after all Closes, got %d", got)
	}
}

// drainEvents pulls up to n events from the subscriber within the
// given timeout. Returns whatever it got — caller asserts on len.
func drainEvents(t *testing.T, sub *Subscriber, n int, timeout time.Duration) []Event {
	t.Helper()
	out := make([]Event, 0, n)
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case e := <-sub.Events():
			out = append(out, e)
		case <-deadline:
			return out
		}
	}
	return out
}

// TestEventBus_RingGrowsThroughAllStagesThenEvicts drives one channel
// through every backing-array transition: empty → initial allocation →
// two doublings (the second clamped by capacity) → eviction. Replay
// after the growth chain must return the surviving window in order —
// proving grow() re-linearizes head/count correctly at each stage.
func TestEventBus_RingGrowsThroughAllStagesThenEvicts(t *testing.T) {
	// 40 sits between growth stages: 16 → 32 → clamp at 40 → evict.
	bus := NewEventBus(40)
	defer bus.Close()

	for i := 1; i <= 45; i++ {
		if _, err := bus.Emit("ch1", i); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	// 45 emits into a 40-cap ring: surviving seqs are 6..45. A client
	// at lastSeq=5 sits exactly on the in-window edge.
	out := bus.Replay(map[string]uint64{"ch1": 5})
	if len(out) != 40 {
		t.Fatalf("replay returned %d events, want 40", len(out))
	}
	for i, e := range out {
		if want := uint64(6 + i); e.Seq != want {
			t.Fatalf("replay[%d].Seq = %d, want %d (growth must preserve order)", i, e.Seq, want)
		}
		if e.Gap {
			t.Fatalf("replay[%d] unexpectedly a gap marker", i)
		}
	}

	// One past the edge gaps.
	out = bus.Replay(map[string]uint64{"ch1": 4})
	if len(out) != 1 || !out[0].Gap {
		t.Fatalf("lastSeq=4 must gap, got %+v", out)
	}
}

// TestEventBus_RingRetainsWireBytesOnly pins the ring's single-copy
// payload retention: replayed events carry no Data (the ring drops it
// at append — retaining it would hold every payload twice) but their
// WireBytes still decode to a complete, correct envelope, which is
// what the replay write path sends verbatim.
func TestEventBus_RingRetainsWireBytesOnly(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	type payload struct {
		Title string `json:"title"`
	}
	if _, err := bus.Emit("ch1", payload{Title: "hello"}); err != nil {
		t.Fatal(err)
	}

	out := bus.Replay(map[string]uint64{"ch1": 0})
	if len(out) != 1 {
		t.Fatalf("replay returned %d events, want 1", len(out))
	}
	if out[0].Data != nil {
		t.Fatalf("ring retained Data (%s); WireBytes already embeds the payload", string(out[0].Data))
	}
	var frame ServerFrame
	if err := json.Unmarshal(out[0].WireBytes, &frame); err != nil {
		t.Fatalf("replayed WireBytes not valid JSON: %v", err)
	}
	var got payload
	if err := json.Unmarshal(frame.Data, &got); err != nil {
		t.Fatalf("envelope Data not valid JSON: %v", err)
	}
	if got.Title != "hello" || frame.Seq != 1 || frame.Channel != "ch1" {
		t.Fatalf("replayed envelope = %+v (payload %+v), want ch1 seq 1 title hello", frame, got)
	}
}

func TestEventBus_LatestOnlyChannel_RetainsOneAndNeverGaps(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	// system:stats is in latestOnlyEventChannels: whole-state frames
	// where the newest supersedes everything prior.
	for i := 1; i <= 5; i++ {
		bus.Emit("system:stats", i)
	}

	// A fresh client (or one that missed any number of frames) gets
	// exactly the newest frame — never a gap marker, which would make
	// it log/recover for state the frame in hand already replaces.
	out := bus.Replay(map[string]uint64{"system:stats": 0})
	if len(out) != 1 || out[0].Gap || out[0].Seq != 5 {
		t.Fatalf("expected single non-gap newest frame seq=5, got %+v", out)
	}
	if string(out[0].WireBytes) == "" {
		t.Fatalf("replayed frame must carry pre-encoded wire bytes")
	}

	// A client already at head gets nothing.
	out = bus.Replay(map[string]uint64{"system:stats": 5})
	if len(out) != 0 {
		t.Fatalf("expected no replay at head, got %+v", out)
	}

	// Sequence numbering is unaffected by the shallow ring.
	evt, err := bus.Emit("system:stats", 6)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if evt.Seq != 6 {
		t.Fatalf("expected seq 6, got %d", evt.Seq)
	}
}

// TestEventBus_EmitWireBytesByteIdentical pins the append-splice
// encoding (appendEventWire) byte-for-byte against the reflection
// marshal it replaced. Channels with JSON-special and multibyte
// characters exercise the cached escaped-channel prefix; payload
// shapes exercise the data splice.
func TestEventBus_EmitWireBytesByteIdentical(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	channels := []string{
		"provider:item_event",
		`ch"quote\slash`,
		"ch<&>html",
		"chанал-ünïcode",
	}
	payloads := []any{
		map[string]any{"title": "hello", "n": 7},
		"plain <string> & escape",
		nil,
		[]int{1, 2, 3},
	}
	for _, ch := range channels {
		for i, p := range payloads {
			evt, err := bus.Emit(ch, p)
			if err != nil {
				t.Fatalf("emit %q #%d: %v", ch, i, err)
			}
			want, err := encodeEventFrame(evt)
			if err != nil {
				t.Fatalf("reference encode %q #%d: %v", ch, i, err)
			}
			if !bytes.Equal(evt.WireBytes, want) {
				t.Fatalf("WireBytes diverged from reflection marshal for %q #%d:\n got %s\nwant %s",
					ch, i, evt.WireBytes, want)
			}
		}
	}
}

// TestEventBus_LiveEventDataAliasesWireBytes pins the single-copy
// contract for live fanout: Data is a subslice of WireBytes (payload
// bytes inside the envelope), so a queued event retains one payload
// buffer, not two, while non-wire subscribers can still decode Data.
func TestEventBus_LiveEventDataAliasesWireBytes(t *testing.T) {
	bus := NewEventBus(10)
	defer bus.Close()

	sub := bus.Subscribe()
	defer sub.Close()

	payload := map[string]string{"k": "v"}
	emitted, err := bus.Emit("ch1", payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, evt := range []Event{emitted, drainEvents(t, sub, 1, time.Second)[0]} {
		if len(evt.Data) == 0 {
			t.Fatal("live event must carry Data (harness consumers decode it)")
		}
		var got map[string]string
		if err := json.Unmarshal(evt.Data, &got); err != nil {
			t.Fatalf("Data does not decode: %v", err)
		}
		if got["k"] != "v" {
			t.Fatalf("Data round-trip = %v", got)
		}
		dataStart := len(evt.WireBytes) - 1 - len(evt.Data)
		if &evt.Data[0] != &evt.WireBytes[dataStart] {
			t.Fatal("Data must alias the payload bytes inside WireBytes (one copy per queued event)")
		}
	}
}

// TestEventBus_ConcurrentEmit_SubscriberSeqOrderedPerChannel pins the
// ordering contract under the narrowed critical section: with one
// emitter goroutine per channel, a subscriber must observe each
// channel's seqs strictly contiguous — restructuring Emit's locking
// must not let ring/seq assignment and fanout race into reordering
// within a channel.
func TestEventBus_ConcurrentEmit_SubscriberSeqOrderedPerChannel(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()

	sub := bus.Subscribe()
	defer sub.Close()

	const chans = 4
	const each = 100 // chans*each < DefaultSubscriberBuffer: no drops
	var wg sync.WaitGroup
	for c := range chans {
		wg.Go(func() {
			channel := fmt.Sprintf("ch%d", c)
			for range each {
				if _, err := bus.Emit(channel, "x"); err != nil {
					t.Errorf("emit %s: %v", channel, err)
					return
				}
			}
		})
	}
	wg.Wait()

	got := drainEvents(t, sub, chans*each, 2*time.Second)
	if len(got) != chans*each {
		t.Fatalf("drained %d events, want %d", len(got), chans*each)
	}
	next := make(map[string]uint64, chans)
	for i, e := range got {
		if e.Seq != next[e.Channel]+1 {
			t.Fatalf("event %d on %s: seq %d after %d (must be contiguous per channel)",
				i, e.Channel, e.Seq, next[e.Channel])
		}
		next[e.Channel] = e.Seq
	}
}

// TestEventBus_SubscriberOriginFilterAtEnqueue pins that an armed
// origin filter keeps invisible channels from ever occupying buffer
// slots, and that the delivered set exactly matches what pump-side
// eventVisibleToOrigin filtering would have produced — with an unset
// filter preserving the deliver-everything default for non-conn
// subscribers.
func TestEventBus_SubscriberOriginFilterAtEnqueue(t *testing.T) {
	channels := []string{
		"terminal:output", // loopback-only
		"highlight:seed",  // remote-only
		"thread:update",   // universal
	}
	cases := []struct {
		name     string
		loopback *bool
	}{
		{"remote", ptrBool(false)},
		{"loopback", ptrBool(true)},
		{"unfiltered", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := NewEventBus(10)
			defer bus.Close()
			sub := bus.Subscribe()
			defer sub.Close()
			if tc.loopback != nil {
				sub.SetOriginLoopback(*tc.loopback)
			}

			want := make(map[string]bool, len(channels))
			for _, ch := range channels {
				if _, err := bus.Emit(ch, "x"); err != nil {
					t.Fatalf("emit %s: %v", ch, err)
				}
				want[ch] = tc.loopback == nil || eventVisibleToOrigin(ch, *tc.loopback)
			}

			got := make(map[string]bool, len(channels))
			for _, e := range drainEvents(t, sub, len(channels), 200*time.Millisecond) {
				got[e.Channel] = true
			}
			for _, ch := range channels {
				if got[ch] != want[ch] {
					t.Errorf("channel %s delivered=%v, want %v", ch, got[ch], want[ch])
				}
			}
		})
	}
}

func ptrBool(v bool) *bool { return &v }
