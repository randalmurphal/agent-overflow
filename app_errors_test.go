package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAppendErrorFiltersNil(t *testing.T) {
	if got := appendError(nil, nil); got != nil {
		t.Fatalf("appendError(nil, nil) = %v, want nil", got)
	}

	start := []error{errors.New("first")}
	got := appendError(start, nil)
	if len(got) != 1 {
		t.Fatalf("len(appendError) = %d, want 1", len(got))
	}

	err := errors.New("second")
	got = appendError(got, err)
	if len(got) != 2 || got[1] != err {
		t.Fatalf("appendError did not append the new error: %v", got)
	}
}

func TestWrapLifecycleErrorPassThroughNil(t *testing.T) {
	if err := wrapLifecycleError("close thing", nil); err != nil {
		t.Fatalf("wrapLifecycleError(..., nil) = %v, want nil", err)
	}
}

func TestWrapLifecycleErrorWrapsWithAction(t *testing.T) {
	base := errors.New("inner cause")
	wrapped := wrapLifecycleError("shutdown db", base)
	if wrapped == nil {
		t.Fatal("wrapLifecycleError returned nil for non-nil cause")
	}
	if !errors.Is(wrapped, base) {
		t.Fatalf("errors.Is(%v, base) = false, want true", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "shutdown db:") {
		t.Fatalf("error = %q, want action prefix", wrapped.Error())
	}
}

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
