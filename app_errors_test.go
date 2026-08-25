package main

import (
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/triage"
)

func TestEmitErrorToThreadRoutesThroughTriageWhenAvailable(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.CreateThread(testThread("thread-xyz")); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	emitted := collectErrorItemUpserts(t, app, 1)

	app.emitErrorToThread("thread-xyz", "boom")

	select {
	case item := <-emitted:
		if item.ThreadID != "thread-xyz" {
			t.Fatalf("item.ThreadID = %q, want thread-xyz", item.ThreadID)
		}
		if item.Summary != "boom" {
			t.Fatalf("item.Summary = %q", item.Summary)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for triaged error item")
	}
}

// TestEmitWireErrorToThreadRespectsStoppedGate pins the routing split:
// errors triggered by wire events on the provider read loop (the
// discussion-sync failure path) must drop with the rest of a stopped
// thread's tail, while host-synthesized errors keep their
// HandleSynthetic carve-out. Without the split, a torn-down session's
// draining read loop could persist items under the stopped thread
// (Bug B5 / invariant 29).
func TestEmitWireErrorToThreadRespectsStoppedGate(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.CreateThread(testThread("thread-stopped")); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

	app.triage.CleanupThread("thread-stopped")

	app.emitWireErrorToThread("thread-stopped", "discussion sync failed: late wire tail")
	items, err := app.store.ListItems("thread-stopped")
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("wire-routed error persisted %d items on a stopped thread, want 0", len(items))
	}

	app.emitErrorToThread("thread-stopped", "reconnect failed: binary not found")
	items, err = app.store.ListItems("thread-stopped")
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].Kind != "error" {
		t.Fatalf("host-synthesized error did not persist through the carve-out: %+v", items)
	}
}

// TestEmitErrorToThreadIsSafeWithoutTriage proves the triage-nil branch
// in emitErrorToThread degrades to a log breadcrumb rather than panicking
// or emitting on a retired wire channel. This path only fires when an
// error surfaces before the triage router is wired at startup, so the
// bar is "no crash, no dead wire emission."
func TestEmitErrorToThreadIsSafeWithoutTriage(t *testing.T) {
	app := &App{}

	var emittedName string
	app.emitEventFn = func(name string, _ any) {
		emittedName = name
	}

	app.emitErrorToThread("thread-abc", "fell back")

	if emittedName != "" {
		t.Fatalf("unexpected emission %q — triage-nil fallback must not touch the wire", emittedName)
	}
}
