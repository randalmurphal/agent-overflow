package transport

import (
	"strings"
	"testing"

	"agent-overflow/internal/eventchan"
)

// The reconnect budget (eventbus.go DefaultRingCapacity): 30 s of a
// 100-agent burst at the rate transport-replay-burst.spec.ts measures on
// provider:item_event, the busiest channel, with the frame size it sends.
const (
	reconnectBudgetSeconds      = 30
	burstItemFramesPerSecond    = 150
	burstItemFrameBytes         = 1024
	reconnectBudgetFrameCount   = reconnectBudgetSeconds * burstItemFramesPerSecond
	reconnectBudgetRetainsBytes = reconnectBudgetFrameCount * burstItemFrameBytes
)

// replayFrom replays one channel from cursor and reports what came back.
func replayFrom(bus *EventBus, channel string, cursor uint64) (events int, gap bool) {
	for _, e := range bus.Replay(map[string]uint64{channel: cursor}) {
		if e.Gap {
			gap = true
			continue
		}
		events++
	}
	return events, gap
}

// TestRingHoldsTheReconnectBudget: the default ring holds the budget's
// burst, and its boundary is exact: a cursor from before the first frame
// replays everything while the ring holds DefaultRingCapacity frames, and
// gaps once one more evicts the first.
func TestRingHoldsTheReconnectBudget(t *testing.T) {
	if DefaultRingCapacity < reconnectBudgetFrameCount {
		t.Fatalf("DefaultRingCapacity %d holds less than the budget's %d frames", DefaultRingCapacity, reconnectBudgetFrameCount)
	}
	if RingByteBudget < reconnectBudgetRetainsBytes {
		t.Fatalf("RingByteBudget %d holds less than the budget's %d bytes", RingByteBudget, reconnectBudgetRetainsBytes)
	}
	bus := NewEventBus(0)
	defer bus.Close()
	channel := string(eventchan.ProviderItemEvent)
	payload := strings.Repeat("x", burstItemFrameBytes-64)
	for range DefaultRingCapacity {
		if _, err := bus.EmitEntity(eventchan.ProviderItemEvent, "thread-A", payload); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	if events, gap := replayFrom(bus, channel, 0); gap || events != DefaultRingCapacity {
		t.Fatalf("at capacity: replayed %d events (gap %v), want all %d", events, gap, DefaultRingCapacity)
	}
	if _, err := bus.EmitEntity(eventchan.ProviderItemEvent, "thread-A", payload); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if _, gap := replayFrom(bus, channel, 0); !gap {
		t.Fatal("one past capacity: a cursor from before the first frame replayed without a gap")
	}
	if events, gap := replayFrom(bus, channel, 1); gap || events != DefaultRingCapacity {
		t.Fatalf("one past capacity: replayed %d events from the second frame (gap %v), want %d", events, gap, DefaultRingCapacity)
	}
}

// TestRingByteBudgetEvictsTheOldest: a ring keeps what fits in its byte
// budget, exactly, and always the newest frame.
func TestRingByteBudgetEvictsTheOldest(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()
	channel := "ring-bytes"
	first, err := bus.Emit(eventchan.Channel(channel), strings.Repeat("a", 200))
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	frame := len(first.WireBytes)
	bus.mu.Lock()
	bus.rings[channel].byteBudget = 3 * frame
	bus.mu.Unlock()
	for range 2 {
		if _, err := bus.Emit(eventchan.Channel(channel), strings.Repeat("a", 200)); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	if events, gap := replayFrom(bus, channel, 0); gap || events != 3 {
		t.Fatalf("at the byte budget: replayed %d events (gap %v), want 3", events, gap)
	}
	if _, err := bus.Emit(eventchan.Channel(channel), strings.Repeat("a", 200)); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if _, gap := replayFrom(bus, channel, 0); !gap {
		t.Fatal("one frame past the byte budget: the first frame was not evicted")
	}
	if events, gap := replayFrom(bus, channel, 1); gap || events != 3 {
		t.Fatalf("one frame past the byte budget: replayed %d events from the second (gap %v), want 3", events, gap)
	}

	big, err := bus.Emit(eventchan.Channel(channel), strings.Repeat("b", 4*frame))
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if events, gap := replayFrom(bus, channel, big.Seq-1); gap || events != 1 {
		t.Fatalf("a frame larger than the budget: replayed %d events (gap %v), want it alone", events, gap)
	}

	bus.DropRetained()
	for range 3 {
		if _, err := bus.Emit(eventchan.Channel(channel), strings.Repeat("a", 200)); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	if events, gap := replayFrom(bus, channel, big.Seq); gap || events != 3 {
		t.Fatalf("after DropRetained: replayed %d events (gap %v), want the 3 emitted since", events, gap)
	}
}
