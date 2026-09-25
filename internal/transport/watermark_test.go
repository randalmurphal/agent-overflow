package transport

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
)

// watermarkBus returns a bus and one subscriber on it watching thread-A.
func watermarkBus(t *testing.T, bus *EventBus) *Subscriber {
	t.Helper()
	fixtureChannel(t, eventchan.ProviderItemEvent, RetentionDefault)
	if !channelEntityFiltered(string(eventchan.ProviderItemEvent)) {
		t.Fatal("provider:item_event is no longer entity-filtered; pick another fixture")
	}
	sub := bus.Subscribe()
	t.Cleanup(sub.Close)
	sub.SetWatch([]string{"thread-A"}, nil, nil)
	return sub
}

func emitFor(t *testing.T, bus *EventBus, channel eventchan.Channel, thread string, payload any) Event {
	t.Helper()
	evt, err := bus.EmitEntity(channel, thread, payload)
	if err != nil {
		t.Fatalf("emit %s for %s: %v", channel, thread, err)
	}
	return evt
}

// drainQueued empties sub's queue and returns the seqs it held.
func drainQueued(sub *Subscriber) []uint64 {
	var seqs []uint64
	for {
		select {
		case e := <-sub.Events():
			seqs = append(seqs, e.Seq)
		default:
			return seqs
		}
	}
}

// TestDeliverMarksOnlyElectedWithholds: a frame the watch set or the
// background lease holds back is marked; one the channel subscription,
// the origin or the grants hold back is not, since those channels never
// reach the client's cursor.
func TestDeliverMarksOnlyElectedWithholds(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	item := eventchan.ProviderItemEvent
	seed := fixtureChannel(t, eventchan.HighlightSeed, RetentionEphemeral)

	watching := watermarkBus(t, bus)
	watching.SetOriginLoopback(false)
	background := bus.Subscribe()
	defer background.Close()
	background.SetOriginLoopback(false)
	background.SetBackground(true)
	subscribed := bus.Subscribe()
	defer subscribed.Close()
	subscribed.SetChannels([]string{"notification:send"})
	subscribed.SetWatch([]string{"thread-A"}, nil, nil)
	loopback := bus.Subscribe()
	defer loopback.Close()
	loopback.SetOriginLoopback(true)
	loopback.SetWatch([]string{"thread-A"}, nil, nil)
	ungranted := bus.Subscribe()
	defer ungranted.Close()
	ungranted.SetScopeFilter(sessionScopeFilter(nil, false))
	ungranted.SetWatch([]string{"thread-A"}, nil, nil)

	emitFor(t, bus, item, "thread-B", "b1")
	lastB := emitFor(t, bus, item, "thread-B", "b2")
	emitFor(t, bus, seed, "thread-A", "seed-a")
	seedB := emitFor(t, bus, seed, "thread-B", "seed-b")

	if got, want := watching.takeWithheld(), map[string]uint64{string(item): lastB.Seq, string(seed): seedB.Seq}; !maps.Equal(got, want) {
		t.Fatalf("watch-withheld marks = %v, want %v", got, want)
	}
	if got, want := background.takeWithheld(), map[string]uint64{string(seed): seedB.Seq}; !maps.Equal(got, want) {
		t.Fatalf("lease-withheld marks = %v, want %v", got, want)
	}
	// The loopback connection is withheld the remote-only seed by origin,
	// before its watch set is consulted: only its item frames are marked.
	if got, want := loopback.takeWithheld(), map[string]uint64{string(item): lastB.Seq}; !maps.Equal(got, want) {
		t.Fatalf("origin-withheld marks = %v, want %v", got, want)
	}
	for name, sub := range map[string]*Subscriber{"subscription": subscribed, "grant": ungranted} {
		if got := sub.takeWithheld(); got != nil {
			t.Fatalf("a %s withhold recorded marks %v", name, got)
		}
	}
	if got := watching.takeWithheld(); got != nil {
		t.Fatalf("a second take returned %v, want nil: the first must swap the set out", got)
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = watching.takeWithheld() }); allocs != 0 {
		t.Fatalf("an empty take allocated %v times", allocs)
	}
}

// TestDeliverMarkNeverPassesALoss: no mark is recorded while the channel
// has an unannounced loss, and a frame that reaches the enqueue attempt
// clears the mark whether it is delivered or dropped.
func TestDeliverMarkNeverPassesALoss(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	bus.subBuf = 1
	sub := watermarkBus(t, bus)
	item := eventchan.ProviderItemEvent

	emitFor(t, bus, item, "thread-A", "a1") // fills the one-slot queue
	emitFor(t, bus, item, "thread-A", "a2") // dropped: the channel is gapped
	emitFor(t, bus, item, "thread-B", "b3")
	if got := sub.takeWithheld(); got != nil {
		t.Fatalf("marks recorded while the channel had an unannounced loss: %v", got)
	}

	drainQueued(sub)
	emitFor(t, bus, item, "thread-A", "a4") // delivered with the gap announcement
	b5 := emitFor(t, bus, item, "thread-B", "b5")
	if got, want := sub.takeWithheld(), map[string]uint64{string(item): b5.Seq}; !maps.Equal(got, want) {
		t.Fatalf("marks after the loss was announced = %v, want %v", got, want)
	}

	drainQueued(sub)
	emitFor(t, bus, item, "thread-B", "b6")
	emitFor(t, bus, item, "thread-A", "a7") // delivered
	if got := sub.takeWithheld(); got != nil {
		t.Fatalf("a delivered frame left marks %v", got)
	}

	emitFor(t, bus, item, "thread-B", "b8")
	emitFor(t, bus, item, "thread-A", "a9") // the queue still holds a7: dropped
	if got := sub.takeWithheld(); got != nil {
		t.Fatalf("a dropped frame left marks %v", got)
	}
}

// TestWatermarkWireShape: a watermark is an event frame with no data key,
// alone and spliced into a batch.
func TestWatermarkWireShape(t *testing.T) {
	mark := watermarkEvent("provider:item_event", 42)
	const want = `{"type":"event","channel":"provider:item_event","seq":42,"watermark":true}`
	if string(mark.WireBytes) != want {
		t.Fatalf("watermark wire = %s, want %s", mark.WireBytes, want)
	}
	var frame ServerFrame
	if err := json.Unmarshal(mark.WireBytes, &frame); err != nil || !frame.Watermark || frame.Seq != 42 || frame.Data != nil {
		t.Fatalf("decoded watermark = %+v (%v)", frame, err)
	}

	bus := NewEventBus(0)
	defer bus.Close()
	evt := emitFor(t, bus, eventchan.ProviderItemEvent, "thread-A", "a1")
	spliced := spliceBatchFrame([]Event{evt, mark})
	wantBatch := `{"type":"batch","events":[` + string(evt.WireBytes) + `,` + want + `]}`
	if string(spliced) != wantBatch {
		t.Fatalf("batch wire = %s, want %s", spliced, wantBatch)
	}
	var batch batchFrame
	if err := json.Unmarshal(spliced, &batch); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	if len(batch.Events) != 2 || batch.Events[0].Watermark || !batch.Events[1].Watermark ||
		batch.Events[1].Seq != 42 || batch.Events[1].Data != nil {
		t.Fatalf("decoded batch = %+v", batch.Events)
	}
}

// pumpWire is a connection pump whose wire is a slice: the coalesce
// buffer's flush appends to it, exactly where writeBatchFrame would write.
type pumpWire struct {
	h      *connHandler
	buf    *coalesceBuffer
	deltas deltaCoalescer
	wire   []Event
}

func newPumpWire(sub *Subscriber) *pumpWire {
	p := &pumpWire{h: &connHandler{sub: sub, profile: connProfile{isLoopback: true}}}
	p.buf = newCoalesceBuffer(DefaultCoalesceMaxEvents, time.Hour, func(batch []Event) {
		p.wire = append(p.wire, batch...)
	})
	p.deltas = deltaCoalescer{window: time.Hour, emit: p.buf.add}
	return p
}

// tick runs the pump's watermark branch and flushes everything it holds.
func (p *pumpWire) tick() []Event {
	p.h.sendWatermarks(p.buf, &p.deltas)
	p.deltas.stop()
	p.buf.flushNow()
	return p.wire
}

func describeWire(wire []Event) []string {
	out := make([]string, 0, len(wire))
	for _, e := range wire {
		label := fmt.Sprintf("%s@%d", e.Channel, e.Seq)
		if e.Watermark {
			label = "watermark:" + label
		}
		out = append(out, label)
	}
	return out
}

// TestPumpWatermarkFollowsQueuedFrames: a frame queued before the mark
// was recorded reaches the wire ahead of the watermark, including a delta
// the background lease is holding for a merge.
func TestPumpWatermarkFollowsQueuedFrames(t *testing.T) {
	item := string(eventchan.ProviderItemEvent)
	t.Run("queued", func(t *testing.T) {
		bus := NewEventBus(0)
		defer bus.Close()
		sub := watermarkBus(t, bus)
		k := emitFor(t, bus, eventchan.ProviderItemEvent, "thread-A", "a1")
		m := emitFor(t, bus, eventchan.ProviderItemEvent, "thread-B", "b2")

		wire := newPumpWire(sub).tick()
		if len(wire) != 2 || wire[0].Seq != k.Seq || wire[0].Watermark || !wire[1].Watermark || wire[1].Seq != m.Seq || wire[1].Channel != item {
			t.Fatalf("wire = %v, want frame %d then watermark %d", describeWire(wire), k.Seq, m.Seq)
		}
	})
	t.Run("merging", func(t *testing.T) {
		bus := NewEventBus(0)
		defer bus.Close()
		sub := watermarkBus(t, bus)
		p := newPumpWire(sub)
		p.h.leaseBackground.Store(true)
		delta := map[string]any{"action": "delta", "threadId": "thread-A", "itemId": "item-1", "delta": "x"}
		emitFor(t, bus, eventchan.ProviderItemEvent, "thread-A", delta)
		k := emitFor(t, bus, eventchan.ProviderItemEvent, "thread-A", delta)
		m := emitFor(t, bus, eventchan.ProviderItemEvent, "thread-B", "b3")

		wire := p.tick()
		if len(wire) != 2 || wire[0].Seq != k.Seq || wire[0].Watermark || !wire[1].Watermark || wire[1].Seq != m.Seq {
			t.Fatalf("wire = %v, want the merged delta at %d then watermark %d", describeWire(wire), k.Seq, m.Seq)
		}
	})
}

// TestPumpWatermarkIsVisibilityGated: a mark on a channel this connection
// may not see sends nothing.
func TestPumpWatermarkIsVisibilityGated(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()
	seed := string(eventchan.HighlightSeed)
	item := string(eventchan.ProviderItemEvent)
	sub.withheld = map[string]uint64{seed: 7, item: 9}
	p := newPumpWire(sub)
	if p.h.eventVisible(seed) || !p.h.eventVisible(item) {
		t.Fatal("fixture: a loopback connection must see provider:item_event and not the remote-only highlight:seed")
	}
	wire := p.tick()
	if len(wire) != 1 || wire[0].Channel != item || wire[0].Seq != 9 || !wire[0].Watermark {
		t.Fatalf("wire = %v, want only the visible channel's watermark", describeWire(wire))
	}
}

// TestWatermarkKeepsReconnectFromGapping is the defect end to end on the
// bus: a client watching thread-A while thread-B emits holds a cursor at
// its last received frame. Once B's frames age out of the ring, that cursor
// gaps; the watermark's cursor does not.
func TestWatermarkKeepsReconnectFromGapping(t *testing.T) {
	clock := newRingClock()
	bus := newEventBus(0, clock.now)
	defer bus.Close()
	sub := watermarkBus(t, bus)
	item := eventchan.ProviderItemEvent

	received := emitFor(t, bus, item, "thread-A", "a1")
	var last Event
	for range 4 {
		last = emitFor(t, bus, item, "thread-B", "b")
	}
	wire := newPumpWire(sub).tick()
	marks := slices.DeleteFunc(slices.Clone(wire), func(e Event) bool { return !e.Watermark })
	if len(marks) != 1 || marks[0].Seq != last.Seq {
		t.Fatalf("watermarks = %v, want one at thread-B's last seq %d", describeWire(marks), last.Seq)
	}

	clock.advance(RingRetainFor + time.Nanosecond)
	bus.sweepAged()
	if out := bus.Replay(map[string]uint64{string(item): marks[0].Seq}); len(out) != 0 {
		t.Fatalf("replay from the watermark = %s, want nothing", describeReplay(out))
	}
	out := bus.Replay(map[string]uint64{string(item): received.Seq})
	if len(out) != 1 || !out[0].Gap {
		t.Fatalf("replay from the last received frame = %s, want the gap the watermark avoids", describeReplay(out))
	}
}

// TestReplayWatermarksWithheldFrames: replay ends with a watermark at the
// newest frame the watch filter held back, when no delivered frame on the
// channel follows it, so a reconnect's cursor does not stay below frames
// withheld during the outage.
func TestReplayWatermarksWithheldFrames(t *testing.T) {
	item := string(eventchan.ProviderItemEvent)
	for _, tc := range []struct {
		name    string
		threads []string
		watch   bool
		want    []uint64
		mark    uint64
	}{
		{"withheldLast", []string{"thread-A", "thread-B", "thread-B"}, true, []uint64{1}, 3},
		{"deliveredLast", []string{"thread-B", "thread-A"}, true, []uint64{2}, 0},
		{"wildcard", []string{"thread-A", "thread-B"}, false, []uint64{1, 2}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServerFixture(t)
			for _, thread := range tc.threads {
				if _, err := f.bus.EmitEntity(eventchan.ProviderItemEvent, thread, thread); err != nil {
					t.Fatalf("emit: %v", err)
				}
			}
			conn := f.dial(t)
			if tc.watch {
				sendFrame(t, conn, ClientFrame{Type: frameTypeWatch, Threads: []string{"thread-A"}})
			}
			replay := requestReplay(t, conn, map[string]uint64{item: 0})
			var got []uint64
			for _, e := range replay.events {
				if e.Gap {
					t.Fatalf("replay announced a gap: %+v", e)
				}
				got = append(got, e.Seq)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("replayed seqs %v, want %v", got, tc.want)
			}
			switch {
			case tc.mark == 0 && len(replay.watermarks) != 0:
				t.Fatalf("watermarks %+v, want none", replay.watermarks)
			case tc.mark != 0 && (len(replay.watermarks) != 1 || replay.watermarks[0].Seq != tc.mark ||
				replay.watermarks[0].Channel != item || replay.watermarks[0].Data != nil):
				t.Fatalf("watermarks %+v, want one on %s at %d", replay.watermarks, item, tc.mark)
			}
		})
	}
}
