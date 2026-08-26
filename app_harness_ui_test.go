package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
)

// newUIQueryHarness builds the smallest Harness that can emit: a zero-value
// App is a silent no-op emitter unless a test hook is installed, which is
// exactly the seam the bridge's correlation needs. Nothing here touches the
// store, the transport or a provider.
func newUIQueryHarness(t *testing.T) (*Harness, <-chan harnessUIQueryEvent) {
	t.Helper()
	emitted := make(chan harnessUIQueryEvent, 8)
	app := &App{}
	app.testEmitHook = func(channel string, data any) {
		if channel != string(eventchan.HarnessUIQuery) {
			return
		}
		event, ok := data.(harnessUIQueryEvent)
		if !ok {
			t.Errorf("harness:ui-query carried %T, want harnessUIQueryEvent", data)
			return
		}
		emitted <- event
	}
	return &Harness{app: app}, emitted
}

func mustEmittedQuery(t *testing.T, emitted <-chan harnessUIQueryEvent) harnessUIQueryEvent {
	t.Helper()
	select {
	case event := <-emitted:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("no harness:ui-query event was emitted")
		return harnessUIQueryEvent{}
	}
}

func TestHarnessUIQueryResolvesOnItsReply(t *testing.T) {
	h, emitted := newUIQueryHarness(t)

	type answer struct {
		result json.RawMessage
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		result, err := h.HarnessUIQuery(json.RawMessage(`{"v":1,"kind":"viewport"}`))
		done <- answer{result: result, err: err}
	}()

	event := mustEmittedQuery(t, emitted)
	if string(event.Spec) != `{"v":1,"kind":"viewport"}` {
		t.Errorf("spec forwarded as %s, want it verbatim", event.Spec)
	}
	if err := h.HarnessUIQueryReply(event.ID, json.RawMessage(`{"v":1,"panes":[]}`)); err != nil {
		t.Fatalf("HarnessUIQueryReply: %v", err)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("HarnessUIQuery: %v", got.err)
	}
	if string(got.result) != `{"v":1,"panes":[]}` {
		t.Errorf("result = %s, want it verbatim", got.result)
	}
	if h.ui.pending() != 0 {
		t.Errorf("%d waiters left parked after an answered query", h.ui.pending())
	}
}

// The timeout message names the two states a caller can actually fix.
func TestHarnessUIQueryTimesOutNamingTheMissingBridge(t *testing.T) {
	h, emitted := newUIQueryHarness(t)

	_, err := h.queryUI(json.RawMessage(`{"v":1,"kind":"viewport"}`), 40*time.Millisecond)
	if err == nil {
		t.Fatal("a query nobody answers must fail")
	}
	if !strings.Contains(err.Error(), "no frontend attached or harness bridge inactive") {
		t.Errorf("timeout error = %q, want it to name the missing bridge", err)
	}
	mustEmittedQuery(t, emitted) // the directive still went out
	if h.ui.pending() != 0 {
		t.Errorf("%d waiters left parked after a timeout", h.ui.pending())
	}
}

// A reply arriving after its caller gave up names an id with no waiter. It
// is dropped silently: the replying bridge did nothing wrong, and erroring
// would turn a lost race into a red test in the frontend.
func TestHarnessUIQueryReplyAfterTimeoutIsDropped(t *testing.T) {
	h, emitted := newUIQueryHarness(t)

	if _, err := h.queryUI(json.RawMessage(`{"v":1,"kind":"viewport"}`), 30*time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
	event := mustEmittedQuery(t, emitted)

	if err := h.HarnessUIQueryReply(event.ID, json.RawMessage(`{"v":1}`)); err != nil {
		t.Errorf("a late reply must be dropped silently, got %v", err)
	}
	if err := h.HarnessUIQueryReply("uq-never-issued", json.RawMessage(`{}`)); err != nil {
		t.Errorf("a reply for an unknown id must be dropped silently, got %v", err)
	}
}

// Several frontends may be attached to one backend. The first reply wins;
// the second finds no waiter and is dropped, rather than blocking on a
// channel nobody is reading or overwriting an answer already returned.
func TestHarnessUIQueryFirstReplyWinsAndTheSecondIsDropped(t *testing.T) {
	h, emitted := newUIQueryHarness(t)

	done := make(chan json.RawMessage, 1)
	go func() {
		result, err := h.HarnessUIQuery(json.RawMessage(`{"v":1,"kind":"viewport"}`))
		if err != nil {
			t.Errorf("HarnessUIQuery: %v", err)
		}
		done <- result
	}()
	event := mustEmittedQuery(t, emitted)

	// Both replies race the same id, exactly as two attached tabs would.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, body := range []string{`{"from":"first"}`, `{"from":"second"}`} {
		wg.Add(1)
		go func(slot int, payload string) {
			defer wg.Done()
			errs[slot] = h.HarnessUIQueryReply(event.ID, json.RawMessage(payload))
		}(i, body)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("reply %d errored: %v", i, err)
		}
	}

	select {
	case result := <-done:
		body := string(result)
		if body != `{"from":"first"}` && body != `{"from":"second"}` {
			t.Errorf("result = %s, want one of the two replies", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the query never resolved")
	}
	if h.ui.pending() != 0 {
		t.Errorf("%d waiters left parked", h.ui.pending())
	}
}

// `{"error": "..."}` is the bridge's one reserved answer shape, so a
// refusal (unknown kind, unwhitelisted global) reads as a failed RPC rather
// than a successful empty one.
func TestHarnessUIQuerySurfacesABridgeError(t *testing.T) {
	h, emitted := newUIQueryHarness(t)

	done := make(chan error, 1)
	go func() {
		_, err := h.HarnessUIQuery(json.RawMessage(`{"v":1,"kind":"nope"}`))
		done <- err
	}()
	event := mustEmittedQuery(t, emitted)
	if err := h.HarnessUIQueryReply(event.ID, json.RawMessage(`{"error":"unknown query kind \"nope\""}`)); err != nil {
		t.Fatalf("HarnessUIQueryReply: %v", err)
	}

	err := <-done
	if err == nil || !strings.Contains(err.Error(), `unknown query kind "nope"`) {
		t.Fatalf("error = %v, want the bridge's own message", err)
	}
}

func TestHarnessUIQueryRefusesMalformedInput(t *testing.T) {
	h, _ := newUIQueryHarness(t)

	if _, err := h.HarnessUIQuery(nil); err == nil {
		t.Error("an empty spec must be refused")
	}
	if _, err := h.HarnessUIQuery(json.RawMessage(`{oops`)); err == nil {
		t.Error("a non-JSON spec must be refused")
	}
	if err := h.HarnessUIQueryReply("  ", json.RawMessage(`{}`)); err == nil {
		t.Error("an empty id must be refused")
	}
	if err := h.HarnessUIQueryReply("uq-1", json.RawMessage(`{oops`)); err == nil {
		t.Error("a non-JSON result must be refused")
	}
}
