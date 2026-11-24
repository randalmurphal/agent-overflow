package transport

import (
	"encoding/json"
	"testing"
	"time"
)

func makeEvent(channel string, seq uint64) Event {
	return Event{
		Channel: channel,
		Seq:     seq,
		Data:    json.RawMessage(`{"v":1}`),
	}
}

// TestCoalesceBuffer_CountThreshold flushes at the count, and the batch
// keeps each event's seq, channel and gap flag as they were added.
func TestCoalesceBuffer_CountThreshold(t *testing.T) {
	var batches [][]Event
	buf := newCoalesceBuffer(3, time.Hour, func(batch []Event) {
		cp := make([]Event, len(batch))
		copy(cp, batch)
		batches = append(batches, cp)
	})

	channels := []string{"provider:item_event", "provider:usage", "thread:updated"}
	buf.add(Event{Channel: channels[0], Seq: 1, Data: json.RawMessage(`{}`), Gap: true})
	buf.add(makeEvent(channels[1], 2))
	if len(batches) != 0 {
		t.Fatal("flushed before reaching count threshold")
	}
	buf.add(makeEvent(channels[2], 3))
	if len(batches) != 1 {
		t.Fatalf("expected 1 batch after threshold, got %d", len(batches))
	}
	if len(batches[0]) != 3 {
		t.Fatalf("expected 3 events in batch, got %d", len(batches[0]))
	}
	for i, e := range batches[0] {
		if e.Seq != uint64(i+1) || e.Channel != channels[i] || e.Gap != (i == 0) {
			t.Fatalf("event %d = seq %d channel %q gap %v, want seq %d channel %q gap %v",
				i, e.Seq, e.Channel, e.Gap, i+1, channels[i], i == 0)
		}
	}
}

func TestCoalesceBuffer_TimerFlush(t *testing.T) {
	var batches [][]Event
	buf := newCoalesceBuffer(100, 20*time.Millisecond, func(batch []Event) {
		cp := make([]Event, len(batch))
		copy(cp, batch)
		batches = append(batches, cp)
	})

	buf.add(makeEvent("a", 1))
	buf.add(makeEvent("a", 2))

	// Wait for timer to fire, then manually flush (simulating the
	// select loop reading timerC).
	<-buf.timerC()
	buf.flushNow()

	if len(batches) != 1 {
		t.Fatalf("expected 1 batch after timer, got %d", len(batches))
	}
	if len(batches[0]) != 2 {
		t.Fatalf("expected 2 events, got %d", len(batches[0]))
	}
}

func TestCoalesceBuffer_ThrottleNotDebounce(t *testing.T) {
	// The timer should start on the first add and NOT reset on
	// subsequent adds. This bounds latency to exactly the window.
	buf := newCoalesceBuffer(100, 30*time.Millisecond, func(batch []Event) {})

	buf.add(makeEvent("a", 1))
	time.Sleep(15 * time.Millisecond)
	buf.add(makeEvent("a", 2))

	// If debounce, the timer would have been reset and would fire
	// 30ms after the second add (~45ms total). With throttle, it
	// fires 30ms after the first add (~30ms total). We're at ~15ms
	// so the timer should fire within ~20ms.
	select {
	case <-buf.timerC():
		// Timer fired from the first add — throttle semantics.
	case <-time.After(25 * time.Millisecond):
		t.Fatal("timer did not fire within expected throttle window")
	}
}

func TestCoalesceBuffer_StopFlushesRemaining(t *testing.T) {
	var batches [][]Event
	buf := newCoalesceBuffer(100, time.Hour, func(batch []Event) {
		cp := make([]Event, len(batch))
		copy(cp, batch)
		batches = append(batches, cp)
	})

	buf.add(makeEvent("a", 1))
	buf.add(makeEvent("a", 2))
	buf.stop()

	if len(batches) != 1 {
		t.Fatalf("expected 1 batch from stop, got %d", len(batches))
	}
	if len(batches[0]) != 2 {
		t.Fatalf("expected 2 events, got %d", len(batches[0]))
	}

	// Double stop is safe and doesn't re-flush.
	buf.stop()
	if len(batches) != 1 {
		t.Fatal("double stop produced an extra flush")
	}
}

func TestCoalesceBuffer_EmptyStopNoFlush(t *testing.T) {
	flushed := false
	buf := newCoalesceBuffer(100, time.Hour, func(batch []Event) {
		flushed = true
	})
	buf.stop()
	if flushed {
		t.Fatal("stop on empty buffer should not call flush")
	}
}

func TestCoalesceBuffer_NilTimerCBlocks(t *testing.T) {
	buf := newCoalesceBuffer(100, time.Hour, func(batch []Event) {})
	if buf.timerC() != nil {
		t.Fatal("timerC should be nil before any add")
	}
}

func TestCoalesceBuffer_ReusesBackingArray(t *testing.T) {
	flushCount := 0
	buf := newCoalesceBuffer(2, time.Hour, func(batch []Event) {
		flushCount++
	})

	buf.add(makeEvent("a", 1))
	buf.add(makeEvent("a", 2))
	if flushCount != 1 {
		t.Fatal("first batch should have flushed")
	}
	capAfterFirst := cap(buf.events)

	buf.add(makeEvent("a", 3))
	buf.add(makeEvent("a", 4))
	if flushCount != 2 {
		t.Fatal("second batch should have flushed")
	}
	if cap(buf.events) != capAfterFirst {
		t.Fatal("backing array was reallocated after flush")
	}
}
