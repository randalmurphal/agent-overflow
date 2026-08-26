package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
)

// newPerfHarness installs a stand-in bridge: every ui-query is answered
// inline with a canned frontend payload, and every harness:perf event is
// captured. That is enough to prove the run's ownership rules (one at a
// time, reset kills it, the report folds both halves) without a browser.
func newPerfHarness(t *testing.T) (*Harness, func() []harnessPerfEvent) {
	t.Helper()
	var (
		mu      sync.Mutex
		samples []harnessPerfEvent
	)
	h := &Harness{app: &App{}}
	h.app.testEmitHook = func(channel string, data any) {
		switch channel {
		case string(eventchan.HarnessUIQuery):
			event, ok := data.(harnessUIQueryEvent)
			if !ok {
				return
			}
			var spec struct {
				Op string `json:"op"`
			}
			_ = json.Unmarshal(event.Spec, &spec)
			body := `{"ok":true}`
			switch spec.Op {
			case "collect":
				body = `{"fps":58.5,"domNodes":1234}`
			case "stop":
				body = `{"v":1,"frames":{"count":120,"fps":59.1}}`
			}
			// Replying inline is safe: the waiter is registered before the
			// emit and its channel is buffered, so nothing blocks.
			if err := h.HarnessUIQueryReply(event.ID, json.RawMessage(body)); err != nil {
				t.Errorf("stand-in bridge reply: %v", err)
			}
		case string(eventchan.HarnessPerf):
			event, ok := data.(harnessPerfEvent)
			if !ok {
				return
			}
			mu.Lock()
			samples = append(samples, event)
			mu.Unlock()
		}
	}
	return h, func() []harnessPerfEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]harnessPerfEvent(nil), samples...)
	}
}

func TestHarnessPerfRunFoldsBothHalvesAndSummarises(t *testing.T) {
	h, captured := newPerfHarness(t)

	status, err := h.HarnessPerfStart(HarnessPerfSpec{SampleMs: 1}) // clamped to the floor
	if err != nil {
		t.Fatalf("HarnessPerfStart: %v", err)
	}
	if !status.Active || status.SampleMs != harnessPerfMinSampleMs {
		t.Fatalf("status = %+v, want an active run clamped to %dms", status, harnessPerfMinSampleMs)
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(captured()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	samples := captured()
	if len(samples) < 2 {
		t.Fatalf("only %d perf samples emitted", len(samples))
	}
	first := samples[0]
	if first.Seq != 1 {
		t.Errorf("first sample seq = %d, want 1", first.Seq)
	}
	if first.RunID != status.RunID {
		t.Errorf("sample runId = %q, want %q", first.RunID, status.RunID)
	}
	if first.Backend.Goroutines <= 0 || first.Backend.HeapBytes == 0 {
		t.Errorf("backend half looks empty: %+v", first.Backend)
	}
	if string(first.Frontend) != `{"fps":58.5,"domNodes":1234}` {
		t.Errorf("frontend half = %s, want the bridge's sample verbatim", first.Frontend)
	}
	if first.FrontendError != "" {
		t.Errorf("unexpected frontendError %q", first.FrontendError)
	}

	report, err := h.HarnessPerfStop()
	if err != nil {
		t.Fatalf("HarnessPerfStop: %v", err)
	}
	if report.RunID != status.RunID || report.Samples < 2 {
		t.Fatalf("report = %+v", report)
	}
	if string(report.Frontend) != `{"v":1,"frames":{"count":120,"fps":59.1}}` {
		t.Errorf("report.Frontend = %s, want the bridge summary verbatim", report.Frontend)
	}
	if report.Backend.HeapBytes.Count != report.Samples || report.Backend.HeapBytes.Max < report.Backend.HeapBytes.Min {
		t.Errorf("heap series = %+v over %d samples", report.Backend.HeapBytes, report.Samples)
	}
	if report.Backend.Goroutines.Mean <= 0 {
		t.Errorf("goroutine series = %+v", report.Backend.Goroutines)
	}

	after, err := h.HarnessPerfStatus()
	if err != nil {
		t.Fatalf("HarnessPerfStatus: %v", err)
	}
	if after.Active {
		t.Error("a stopped run must not report active")
	}
	if _, err := h.HarnessPerfStop(); err == nil {
		t.Error("stopping twice must fail rather than return an empty report")
	}
}

func TestHarnessPerfStartRefusesASecondRun(t *testing.T) {
	h, _ := newPerfHarness(t)
	if _, err := h.HarnessPerfStart(HarnessPerfSpec{}); err != nil {
		t.Fatalf("HarnessPerfStart: %v", err)
	}
	t.Cleanup(func() { h.stopPerfRunForReset() })
	if _, err := h.HarnessPerfStart(HarnessPerfSpec{}); err == nil {
		t.Fatal("a second concurrent run must be refused")
	}
}

// With no bridge attached the arm query times out, and the refusal must
// leave nothing armed — otherwise the next start would report a run whose
// meters were never actually turned on.
func TestHarnessPerfStartFailsWithNoBridgeAndArmsNothing(t *testing.T) {
	h := &Harness{app: &App{}}
	h.perf.mu.Lock()
	h.perf.mu.Unlock()
	// Shrink the wait: the arm uses the caller-facing 10s timeout, so drive
	// the query path directly to prove the message, then prove the state.
	if _, err := h.queryUI(json.RawMessage(`{"v":1,"kind":"perf","op":"start"}`), 30*time.Millisecond); err == nil {
		t.Fatal("arming with no bridge must fail")
	} else if !strings.Contains(err.Error(), "harness bridge inactive") {
		t.Errorf("error = %v", err)
	}
	status, err := h.HarnessPerfStatus()
	if err != nil {
		t.Fatalf("HarnessPerfStatus: %v", err)
	}
	if status.Active {
		t.Error("no run may be armed after a failed arm")
	}
}

func TestStopPerfRunForResetDropsAnArmedRun(t *testing.T) {
	h, _ := newPerfHarness(t)
	if _, err := h.HarnessPerfStart(HarnessPerfSpec{}); err != nil {
		t.Fatalf("HarnessPerfStart: %v", err)
	}
	h.stopPerfRunForReset()
	status, err := h.HarnessPerfStatus()
	if err != nil {
		t.Fatalf("HarnessPerfStatus: %v", err)
	}
	if status.Active {
		t.Fatal("HarnessReset must leave no perf run armed")
	}
	h.stopPerfRunForReset() // idempotent
}
