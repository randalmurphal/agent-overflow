package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/transport"
)

// TestAppEmitNoOpWhenNeitherBusNorHook verifies the silent-no-op
// boot-path contract: an App with neither a wired transport bus nor
// a test hook drops emissions without panicking. The pre-Startup boot
// path relies on this so callers don't have to nil-check the bus.
func TestAppEmitNoOpWhenNeitherBusNorHook(t *testing.T) {
	a := &App{}
	// Should not panic and should not record anything anywhere — there's
	// no observer to crash on, so the only failure mode is a panic.
	a.emit("test:seq", provider.ProviderEvent{ThreadID: "t1"})
}

// TestAppEmitDeliversRawDataToTestHook verifies the test-hook contract:
// data arrives unwrapped (no envelope) so tests can assert on the
// payload directly. The transport bus assigns its own per-channel seq
// when given a real bus; tests that don't need wire seq use the hook.
func TestAppEmitDeliversRawDataToTestHook(t *testing.T) {
	type record struct {
		name string
		data any
	}
	var captured []record
	a := &App{}
	a.testEmitHook = func(name string, data any) {
		captured = append(captured, record{name: name, data: data})
	}

	want := provider.ProviderEvent{ThreadID: "t1", Kind: provider.EventTextDelta, Content: "hi"}
	a.emit("test:seq", want)

	if len(captured) != 1 {
		t.Fatalf("captured %d emits, want 1", len(captured))
	}
	if captured[0].name != "test:seq" {
		t.Fatalf("name = %q, want test:seq", captured[0].name)
	}
	got, ok := captured[0].data.(provider.ProviderEvent)
	if !ok {
		t.Fatalf("data type = %T, want provider.ProviderEvent (raw, no envelope)", captured[0].data)
	}
	if got.ThreadID != "t1" || got.Content != "hi" {
		t.Fatalf("payload corrupted: %+v", got)
	}
}

// TestAppEmitDeliversThroughTransportBus verifies the production wire
// contract: a real transport bus stamps a per-channel monotonic seq
// and the data round-trips as the original payload. This is the
// invariant frontend subscribers depend on for gap detection.
func TestAppEmitDeliversThroughTransportBus(t *testing.T) {
	bus := transport.NewEventBus(0)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()

	a := &App{}
	a.SetEventBus(bus)

	for i := 1; i <= 5; i++ {
		a.emit("test:seq", provider.ProviderEvent{
			ThreadID: "t1",
			Kind:     provider.EventTextDelta,
			Content:  fmt.Sprintf("chunk-%d", i),
		})
	}

	// Drain the live deliveries the bus pushed to the subscriber.
	got := make([]transport.Event, 0, 5)
	for i := 0; i < 5; i++ {
		select {
		case evt := <-sub.Events():
			got = append(got, evt)
		case <-sub.Done():
			t.Fatal("subscriber closed before draining 5 events")
		}
	}
	for i, evt := range got {
		if evt.Channel != "test:seq" {
			t.Fatalf("event %d channel = %q, want test:seq", i, evt.Channel)
		}
		if evt.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d (per-channel monotonic)", i, evt.Seq, i+1)
		}
		var payload provider.ProviderEvent
		if err := json.Unmarshal(evt.Data, &payload); err != nil {
			t.Fatalf("event %d data: %v", i, err)
		}
		if payload.ThreadID != "t1" {
			t.Fatalf("event %d ThreadID lost: %+v", i, payload)
		}
	}
}

// TestEmitWithReplayDeliversThroughTransportBus pins the
// emitWithReplay path: emissions that flow through the triage router's
// emit closure also reach the transport bus with raw payload (no
// envelope) and the per-channel seq advances.
func TestEmitWithReplayDeliversThroughTransportBus(t *testing.T) {
	bus := transport.NewEventBus(0)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()

	a := &App{}
	a.SetEventBus(bus)

	emit := a.emitWithReplay()
	for i := 1; i <= 3; i++ {
		emit("test:seq", provider.ProviderEvent{
			ThreadID: "t1",
			Kind:     provider.EventTextDelta,
			Content:  fmt.Sprintf("chunk-%d", i),
		})
	}

	for i := 1; i <= 3; i++ {
		select {
		case evt := <-sub.Events():
			if evt.Seq != uint64(i) {
				t.Fatalf("event %d seq = %d, want %d", i, evt.Seq, i)
			}
		case <-sub.Done():
			t.Fatal("subscriber closed before draining 3 events")
		}
	}
}

// TestAppEmitTransportBusPerChannelSeq guards the per-channel scoping
// of the wire seq — different channels each have their own counter.
// This is the transport bus's contract; the App side doesn't track a
// global counter anymore.
func TestAppEmitTransportBusPerChannelSeq(t *testing.T) {
	bus := transport.NewEventBus(0)
	defer bus.Close()
	sub := bus.Subscribe()
	defer sub.Close()

	a := &App{}
	a.SetEventBus(bus)

	a.emit("ch:a", provider.ProviderEvent{ThreadID: "t1"})
	a.emit("ch:b", provider.ProviderEvent{ThreadID: "t1"})
	a.emit("ch:a", provider.ProviderEvent{ThreadID: "t1"})

	type seqEntry struct {
		channel string
		seq     uint64
	}
	var got []seqEntry
	for i := 0; i < 3; i++ {
		select {
		case evt := <-sub.Events():
			got = append(got, seqEntry{channel: evt.Channel, seq: evt.Seq})
		case <-sub.Done():
			t.Fatal("subscriber closed early")
		}
	}
	want := []seqEntry{
		{channel: "ch:a", seq: 1},
		{channel: "ch:b", seq: 1},
		{channel: "ch:a", seq: 2},
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("event %d = %+v, want %+v (per-channel seq)", i, got[i], w)
		}
	}
}
