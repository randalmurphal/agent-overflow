package main

import (
	"testing"
	"time"
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
