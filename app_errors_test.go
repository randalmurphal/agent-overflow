package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
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

func TestEmitProviderEventPrefersTestInjection(t *testing.T) {
	app := &App{}

	var seen provider.ProviderEvent
	app.emitProviderEventFn = func(evt provider.ProviderEvent) {
		seen = evt
	}

	want := provider.ProviderEvent{
		Kind:     provider.EventThreadRenamed,
		ThreadID: "t1",
		Content:  "renamed",
	}
	app.emitProviderEvent(want)

	if seen.ThreadID != want.ThreadID || seen.Content != want.Content {
		t.Fatalf("seen = %+v, want %+v", seen, want)
	}
}

func TestEmitErrorToThreadRoutesThroughTriageWhenAvailable(t *testing.T) {
	app := newTestAppWithStore(t)

	emitted := make(chan provider.ProviderEvent, 1)
	app.triage = triage.NewRouter(app.store, func(eventName string, data any) {
		if eventName != "provider:event" {
			return
		}
		evt, ok := data.(provider.ProviderEvent)
		if !ok {
			t.Fatalf("provider:event payload type = %T", data)
		}
		if evt.Kind == provider.EventError {
			emitted <- evt
		}
	})

	app.emitErrorToThread("thread-xyz", "boom")

	select {
	case evt := <-emitted:
		if evt.ThreadID != "thread-xyz" {
			t.Fatalf("evt.ThreadID = %q, want thread-xyz", evt.ThreadID)
		}
		if evt.Content != "boom" {
			t.Fatalf("evt.Content = %q", evt.Content)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for triaged error event")
	}
}

func TestEmitErrorToThreadFallsBackToProviderEmitWhenNoTriage(t *testing.T) {
	app := &App{}

	emitted := make(chan provider.ProviderEvent, 1)
	app.emitProviderEventFn = func(evt provider.ProviderEvent) {
		emitted <- evt
	}

	app.emitErrorToThread("thread-abc", "fell back")

	select {
	case evt := <-emitted:
		if evt.Kind != provider.EventError || evt.Content != "fell back" {
			t.Fatalf("evt = %+v, want EventError with content", evt)
		}
		if evt.ThreadID != "thread-abc" {
			t.Fatalf("evt.ThreadID = %q", evt.ThreadID)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for fallback emit")
	}
}
