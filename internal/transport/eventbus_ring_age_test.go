package transport

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"agent-overflow/internal/eventchan"
)

// ringClock is a bus clock the test advances by hand. It counts reads,
// since every Emit stamp and every sweep reads it once.
type ringClock struct {
	mu    sync.Mutex
	at    time.Time
	reads int
}

func newRingClock() *ringClock {
	return &ringClock{at: time.Unix(1_800_000_000, 0)}
}

func (c *ringClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.at
}

func (c *ringClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func (c *ringClock) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// ringView is one ring's state, read under the bus lock.
type ringView struct {
	count, bytes, head, slots int
	backingNil, stampsNil     bool
	seq, dropped              uint64
}

func viewRing(t *testing.T, bus *EventBus, channel eventchan.Channel) ringView {
	t.Helper()
	bus.mu.RLock()
	defer bus.mu.RUnlock()
	r := bus.rings[string(channel)]
	if r == nil {
		t.Fatalf("%s has no ring", channel)
	}
	return ringView{
		count: r.count, bytes: r.bytes, head: r.head, slots: len(r.backing),
		backingNil: r.backing == nil, stampsNil: r.stamps == nil,
		seq: r.seq, dropped: r.dropped,
	}
}

// fixtureChannel returns channel after checking it still has the
// retention the test depends on.
func fixtureChannel(t *testing.T, channel eventchan.Channel, retention Retention) eventchan.Channel {
	t.Helper()
	if got := channelRetention(string(channel)); got != retention {
		t.Fatalf("%s is %v, not %v; pick another fixture", channel, got, retention)
	}
	return channel
}

func emitOn(t *testing.T, bus *EventBus, channel eventchan.Channel) Event {
	t.Helper()
	evt, err := bus.EmitEntity(channel, "thread-A", "frame")
	if err != nil {
		t.Fatalf("emit %s: %v", channel, err)
	}
	return evt
}

func wireBytes(events ...Event) int {
	total := 0
	for _, e := range events {
		total += len(e.WireBytes)
	}
	return total
}

// describeReplay renders replayed frames as their seqs, a gap marker as
// gap@seq.
func describeReplay(out []Event) string {
	parts := make([]string, 0, len(out))
	for _, e := range out {
		if e.Gap {
			parts = append(parts, fmt.Sprintf("gap@%d", e.Seq))
			continue
		}
		parts = append(parts, strconv.FormatUint(e.Seq, 10))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// TestRingRetainForCoversTheReconnectBudget: aging never releases a frame
// the reconnect budget (eventbus_ring_budget_test.go) promises to replay.
func TestRingRetainForCoversTheReconnectBudget(t *testing.T) {
	if RingRetainFor < reconnectBudgetSeconds*time.Second {
		t.Fatalf("RingRetainFor %v is shorter than the %d s reconnect budget", RingRetainFor, reconnectBudgetSeconds)
	}
}

// TestRingAppendReleasesAgedFrames: an append first releases the frames
// older than RingRetainFor, and when that empties the ring it frees the
// grown backing and starts over at the initial size.
func TestRingAppendReleasesAgedFrames(t *testing.T) {
	clock := newRingClock()
	bus := newEventBus(0, clock.now)
	defer bus.Close()
	item := fixtureChannel(t, eventchan.ProviderItemEvent, RetentionDefault)

	for range 40 {
		emitOn(t, bus, item)
	}
	if v := viewRing(t, bus, item); v.count != 40 || v.slots != 64 {
		t.Fatalf("after the burst: count %d in %d slots, want 40 in 64", v.count, v.slots)
	}
	clock.advance(3 * time.Minute)
	young := []Event{emitOn(t, bus, item), emitOn(t, bus, item)}
	clock.advance(2*time.Minute + time.Nanosecond)
	young = append(young, emitOn(t, bus, item))

	v := viewRing(t, bus, item)
	if v.count != 3 || v.bytes != wireBytes(young...) {
		t.Fatalf("after the burst aged: count %d, bytes %d; want 3 frames of %d bytes", v.count, v.bytes, wireBytes(young...))
	}
	if v.seq != 43 || v.dropped != 0 {
		t.Fatalf("aging moved seq %d or dropped %d; want 43 and 0", v.seq, v.dropped)
	}

	clock.advance(RingRetainFor + time.Nanosecond)
	last := emitOn(t, bus, item)
	v = viewRing(t, bus, item)
	if v.count != 1 || v.bytes != wireBytes(last) || v.slots != ringInitialCapacity {
		t.Fatalf("after every frame aged: count %d, bytes %d, %d slots; want the new frame alone in %d slots",
			v.count, v.bytes, v.slots, ringInitialCapacity)
	}
}

// TestRingGrowKeepsStampsInStep: grow moves each frame's stamp with it,
// including from a wrapped ring, so aging releases exactly the old frames.
func TestRingGrowKeepsStampsInStep(t *testing.T) {
	clock := newRingClock()
	bus := newEventBus(0, clock.now)
	defer bus.Close()
	item := fixtureChannel(t, eventchan.ProviderItemEvent, RetentionDefault)

	clock.advance(time.Minute)
	for range 40 {
		emitOn(t, bus, item) // seq 1-40 at 1m, across two grows
	}
	clock.advance(3 * time.Minute)
	emitOn(t, bus, item) // seq 41-42 at 4m
	emitOn(t, bus, item)
	clock.advance(time.Minute + time.Nanosecond)
	emitOn(t, bus, item) // seq 43 at 5m+1ns, when the burst is 4m+1ns old
	if v := viewRing(t, bus, item); v.count != 43 {
		t.Fatalf("ring holds %d frames before the burst aged, want 43", v.count)
	}
	clock.advance(time.Minute)
	emitOn(t, bus, item) // seq 44 at 6m+1ns releases the burst
	if v := viewRing(t, bus, item); v.count != 4 || v.head != 40 || v.slots != 64 {
		t.Fatalf("after the burst aged: %+v, want 4 frames from slot 40 of 64", v)
	}
	for range 61 {
		emitOn(t, bus, item) // seq 45-105; the last grows the wrapped ring
	}
	if v := viewRing(t, bus, item); v.count != 65 || v.slots != 128 {
		t.Fatalf("after wrapping: %+v, want 65 frames in 128 slots", v)
	}
	clock.advance(3 * time.Minute)
	emitOn(t, bus, item) // seq 106 at 9m+1ns releases seq 41-42 only
	if v := viewRing(t, bus, item); v.count != 64 {
		t.Fatalf("ring holds %d frames, want seq 43-106", v.count)
	}
	if events, gap := replayFrom(bus, string(item), 42); gap || events != 64 {
		t.Fatalf("replay from 42 = %d events (gap %v), want seq 43-106", events, gap)
	}
	if _, gap := replayFrom(bus, string(item), 41); !gap {
		t.Fatal("replay from 41 did not gap after seq 42 aged")
	}
}

// TestRingSweepReleasesAgedFrames: the sweep releases, on every aging ring,
// the frames strictly older than RingRetainFor, frees the backing of a
// ring it empties, and leaves ephemeral rings as they were.
func TestRingSweepReleasesAgedFrames(t *testing.T) {
	clock := newRingClock()
	bus := newEventBus(0, clock.now)
	defer bus.Close()
	item := fixtureChannel(t, eventchan.ProviderItemEvent, RetentionDefault)
	quiet := fixtureChannel(t, eventchan.ProviderTurnCompleted, RetentionDefault)
	ephemeral := fixtureChannel(t, eventchan.HighlightSeed, RetentionEphemeral)

	for range 40 {
		emitOn(t, bus, item)
	}
	for range 5 {
		emitOn(t, bus, quiet)
	}
	for range 3 {
		emitOn(t, bus, ephemeral)
	}
	clock.advance(3 * time.Minute)
	young := []Event{emitOn(t, bus, item), emitOn(t, bus, item)}

	clock.advance(2 * time.Minute)
	bus.sweepAged()
	if v := viewRing(t, bus, item); v.count != 42 {
		t.Fatalf("at exactly RingRetainFor: item ring holds %d frames, want all 42", v.count)
	}
	if v := viewRing(t, bus, quiet); v.count != 5 {
		t.Fatalf("at exactly RingRetainFor: quiet ring holds %d frames, want all 5", v.count)
	}

	clock.advance(time.Nanosecond)
	bus.sweepAged()
	if v := viewRing(t, bus, item); v.count != 2 || v.bytes != wireBytes(young...) || v.seq != 42 || v.dropped != 0 {
		t.Fatalf("item ring after the sweep = %+v, want the 2 young frames (%d bytes) at seq 42", v, wireBytes(young...))
	}
	v := viewRing(t, bus, quiet)
	if v.count != 0 || v.bytes != 0 || v.head != 0 || !v.backingNil || !v.stampsNil || v.seq != 5 || v.dropped != 0 {
		t.Fatalf("quiet ring after the sweep = %+v, want empty with nil backing at seq 5", v)
	}
	if v := viewRing(t, bus, ephemeral); v.count != 0 || v.seq != 3 || !v.backingNil {
		t.Fatalf("ephemeral ring after the sweep = %+v, want seq 3 and nothing retained", v)
	}
	if out := bus.Replay(map[string]uint64{string(ephemeral): 1}); len(out) != 0 {
		t.Fatalf("ephemeral replay after the sweep = %s, want nothing (no retention, no gap)", describeReplay(out))
	}

	clock.advance(RingRetainFor)
	bus.sweepAged()
	if v := viewRing(t, bus, item); v.count != 0 || v.bytes != 0 || !v.backingNil || !v.stampsNil {
		t.Fatalf("item ring after its last frames aged = %+v, want empty with nil backing", v)
	}
}

// TestRingReplayAfterAging: a cursor at the head replays nothing, one
// below a released frame gets the gap marker at the ring's seq, and one
// above the head keeps its marker, whether aging left frames or none.
func TestRingReplayAfterAging(t *testing.T) {
	clock := newRingClock()
	bus := newEventBus(0, clock.now)
	defer bus.Close()
	item := fixtureChannel(t, eventchan.ProviderItemEvent, RetentionDefault)
	replay := func(cursor uint64) []Event {
		return bus.Replay(map[string]uint64{string(item): cursor})
	}
	wantGap := func(label string, out []Event, seq uint64) {
		t.Helper()
		if len(out) != 1 || !out[0].Gap || out[0].Seq != seq {
			t.Fatalf("%s: replay = %s, want one gap marker at seq %d", label, describeReplay(out), seq)
		}
	}

	for range 3 {
		emitOn(t, bus, item)
	}
	clock.advance(3 * time.Minute)
	emitOn(t, bus, item)
	emitOn(t, bus, item)
	clock.advance(2*time.Minute + time.Nanosecond)
	bus.sweepAged()

	if out := replay(5); len(out) != 0 {
		t.Fatalf("cursor at the head: replay = %s, want nothing", describeReplay(out))
	}
	if out := replay(3); len(out) != 2 || out[0].Gap || out[0].Seq != 4 || out[1].Seq != 5 {
		t.Fatalf("cursor at the last released frame: replay = %s, want seq 4 and 5", describeReplay(out))
	}
	wantGap("cursor below a released frame", replay(2), 5)
	wantGap("cursor above the head", replay(6), 5)

	clock.advance(RingRetainFor)
	bus.sweepAged()
	if v := viewRing(t, bus, item); v.count != 0 {
		t.Fatalf("ring holds %d frames, want every frame aged", v.count)
	}
	if out := replay(5); len(out) != 0 {
		t.Fatalf("emptied ring, cursor at the head: replay = %s, want nothing", describeReplay(out))
	}
	wantGap("emptied ring, cursor below a released frame", replay(4), 5)
	wantGap("emptied ring, zero cursor", replay(0), 5)
	wantGap("emptied ring, cursor above the head", replay(6), 5)

	next := emitOn(t, bus, item)
	if next.Seq != 6 {
		t.Fatalf("emit after aging got seq %d, want 6", next.Seq)
	}
	if out := replay(5); len(out) != 1 || out[0].Gap || out[0].Seq != 6 {
		t.Fatalf("cursor at the old head after a new frame: replay = %s, want seq 6", describeReplay(out))
	}
}

// TestRingLatestOnlyFrameDoesNotAge: a latest-only ring's frame is the
// channel's current value, so it outlives RingRetainFor and replay keeps
// serving it instead of a gap.
func TestRingLatestOnlyFrameDoesNotAge(t *testing.T) {
	clock := newRingClock()
	bus := newEventBus(0, clock.now)
	defer bus.Close()
	stats := fixtureChannel(t, eventchan.SystemStats, RetentionLatestOnly)

	emitOn(t, bus, stats)
	current := emitOn(t, bus, stats)
	clock.advance(10 * RingRetainFor)
	bus.sweepAged()

	if v := viewRing(t, bus, stats); v.count != 1 || v.bytes != wireBytes(current) {
		t.Fatalf("latest-only ring after the sweep = %+v, want its current frame", v)
	}
	out := bus.Replay(map[string]uint64{string(stats): 0})
	if len(out) != 1 || out[0].Gap || out[0].Seq != current.Seq {
		t.Fatalf("replay = %s, want the current frame at seq %d", describeReplay(out), current.Seq)
	}
}

// TestRingSweeperRunsEveryRingSweepEvery: the sweeper NewEventBus starts
// reads time.Now and releases a frame at the first tick after it is older
// than RingRetainFor, with no emit to trigger it.
func TestRingSweeperRunsEveryRingSweepEvery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bus := NewEventBus(0)
		defer bus.Close()
		item := fixtureChannel(t, eventchan.ProviderItemEvent, RetentionDefault)
		emitOn(t, bus, item)

		time.Sleep(RingRetainFor)
		synctest.Wait()
		if v := viewRing(t, bus, item); v.count != 1 {
			t.Fatalf("at RingRetainFor: ring holds %d frames, want 1", v.count)
		}
		time.Sleep(RingSweepEvery)
		synctest.Wait()
		if v := viewRing(t, bus, item); v.count != 0 || !v.backingNil {
			t.Fatalf("one sweep later: ring = %+v, want empty with nil backing", v)
		}
	})
}

// TestRingSweepInterleavesWithEmit: sweeps and emits share the ring under
// the bus lock. A channel emitting every 10 s keeps exactly the frames of
// the last RingRetainFor.
func TestRingSweepInterleavesWithEmit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bus := NewEventBus(0)
		defer bus.Close()
		item := fixtureChannel(t, eventchan.ProviderItemEvent, RetentionDefault)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 90 {
				if _, err := bus.EmitEntity(item, "thread-A", "frame"); err != nil {
					t.Errorf("emit: %v", err)
					return
				}
				time.Sleep(10 * time.Second)
			}
		}()
		<-done
		synctest.Wait()
		// At 900 s the sweep releases frames stamped before 600 s and keeps
		// the 30 emitted from 600 s to 890 s.
		if v := viewRing(t, bus, item); v.count != 30 || v.seq != 90 {
			t.Fatalf("ring after 15 minutes = %+v, want the last 30 of 90 frames", v)
		}
	})
}

// TestEventBusCloseStopsSweeper: Close returns after the sweeper has
// exited, no sweep runs afterwards, a second Close is a no-op, and a late
// Emit neither stamps nor creates a ring.
func TestEventBusCloseStopsSweeper(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := newRingClock()
		bus := newEventBus(0, clock.now)
		defer bus.Close() // Idempotent: stops the sweeper if an assertion ends the test early.
		reads := clock.readCount()
		time.Sleep(RingSweepEvery)
		synctest.Wait()
		if got := clock.readCount(); got != reads+1 {
			t.Fatalf("one tick read the clock %d times, want 1 sweep", got-reads)
		}

		bus.Close()
		select {
		case <-bus.sweepDone:
		default:
			t.Fatal("Close returned before the sweeper exited")
		}
		reads = clock.readCount()
		time.Sleep(10 * RingSweepEvery)
		synctest.Wait()
		if got := clock.readCount(); got != reads {
			t.Fatalf("%d sweeps ran after Close", got-reads)
		}

		bus.Close()
		evt, err := bus.Emit(eventchan.ProviderItemEvent, "late")
		if err != nil || evt.Seq != 0 {
			t.Fatalf("Emit after Close = (%+v, %v), want a no-op", evt, err)
		}
		bus.mu.RLock()
		rings := len(bus.rings)
		bus.mu.RUnlock()
		if rings != 0 || clock.readCount() != reads {
			t.Fatalf("Emit after Close created %d rings and read the clock %d times", rings, clock.readCount()-reads)
		}
	})
}
