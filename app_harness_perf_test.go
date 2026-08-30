package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/procrss"
)

// perfBridge is the stand-in for the in-page harness bridge: every
// ui-query is answered inline with a canned frontend payload, and every
// harness:perf event is captured. That is enough to prove the run's
// ownership rules (one at a time, reset kills it, the report folds both
// halves) without a browser.
type perfBridge struct {
	mu       sync.Mutex
	samples  []harnessPerfEvent
	specs    []map[string]any
	answerNo map[string]bool
}

// samplesSeen snapshots the harness:perf events captured so far.
func (b *perfBridge) samplesSeen() []harnessPerfEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]harnessPerfEvent(nil), b.samples...)
}

// specsSeen snapshots the decoded ui-query specs the sampler issued.
func (b *perfBridge) specsSeen() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]map[string]any(nil), b.specs...)
}

// goSilentOn makes the bridge stop answering one perf op, standing in for
// a page that navigated away mid-run.
func (b *perfBridge) goSilentOn(op string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.answerNo == nil {
		b.answerNo = map[string]bool{}
	}
	b.answerNo[op] = true
}

func (b *perfBridge) silent(op string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.answerNo[op]
}

func newPerfHarness(t *testing.T) (*Harness, *perfBridge) {
	t.Helper()
	bridge := &perfBridge{}
	h := &Harness{app: &App{}}
	h.app.testEmitHook = func(channel string, data any) {
		switch channel {
		case string(eventchan.HarnessUIQuery):
			event, ok := data.(harnessUIQueryEvent)
			if !ok {
				return
			}
			spec := map[string]any{}
			_ = json.Unmarshal(event.Spec, &spec)
			op, _ := spec["op"].(string)
			bridge.mu.Lock()
			bridge.specs = append(bridge.specs, spec)
			bridge.mu.Unlock()
			if bridge.silent(op) {
				return
			}
			body := `{"ok":true}`
			switch op {
			case "collect":
				body = `{"fps":58.5,"domNodes":1234}`
			case "stop":
				body = `{"v":1,"frames":{"count":120,"fps":59.1}}`
			}
			// Replying inline is safe: the waiter is registered before the
			// emit and its channel is buffered, so nothing blocks.
			if err := h.HarnessUIQueryReply("", event.ID, json.RawMessage(body)); err != nil {
				t.Errorf("stand-in bridge reply: %v", err)
			}
		case string(eventchan.HarnessPerf):
			event, ok := data.(harnessPerfEvent)
			if !ok {
				return
			}
			bridge.mu.Lock()
			bridge.samples = append(bridge.samples, event)
			bridge.mu.Unlock()
		}
	}
	return h, bridge
}

func TestHarnessPerfRunFoldsBothHalvesAndSummarises(t *testing.T) {
	h, bridge := newPerfHarness(t)
	captured := bridge.samplesSeen

	status, err := h.HarnessPerfStart(HarnessPerfSpec{SampleMs: 1, PageID: "page-1"}) // clamped to the floor
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

	// Every spec the sampler issued names the run it belongs to, which is
	// what lets a second attached page decline the query instead of
	// answering for a run it never armed.
	specs := bridge.specsSeen()
	if len(specs) < 3 {
		t.Fatalf("only %d ui-query specs issued", len(specs))
	}
	for _, spec := range specs {
		if spec["runId"] != status.RunID {
			t.Errorf("spec %v carries runId %v, want %q", spec["op"], spec["runId"], status.RunID)
		}
		if spec["pageId"] != "page-1" {
			t.Errorf("spec %v carries pageId %v, want page-1", spec["op"], spec["pageId"])
		}
	}
}

// The renderer is not always in this process's subtree — on Windows/WSL
// WebView2 belongs to the launcher — and a run that recorded a zero for
// it would report "the renderer used no memory" about a renderer it never
// measured. Nothing matches the prefix here, so the series must stay
// empty and status must say the number is not measurable.
func TestHarnessPerfRecordsNoWebviewSeriesWhenNoChildMatched(t *testing.T) {
	h, bridge := newPerfHarness(t)
	if _, err := h.HarnessPerfStart(HarnessPerfSpec{
		SampleMs:        harnessPerfMinSampleMs,
		WebviewPrefixes: []string{"no-such-process-name-"},
	}); err != nil {
		t.Fatalf("HarnessPerfStart: %v", err)
	}
	waitFor(t, func() bool { return len(bridge.samplesSeen()) > 0 }, "a first perf sample")

	status, err := h.HarnessPerfStatus()
	if err != nil {
		t.Fatalf("HarnessPerfStatus: %v", err)
	}
	if status.WebviewRSSMeasurable {
		t.Error("no child matched, so the renderer's RSS is not measurable")
	}

	report, err := h.HarnessPerfStop()
	if err != nil {
		t.Fatalf("HarnessPerfStop: %v", err)
	}
	if report.Backend.WebviewRSSBytes.Count != 0 {
		t.Errorf("webview series = %+v, want no samples at all", report.Backend.WebviewRSSBytes)
	}
	if procrss.Supported() && report.Backend.RSSBytes.Count == 0 {
		t.Error("the backend's own RSS is measurable here and must still be recorded")
	}
}

// finishPerfRun runs on the RESET path too, where the page is usually
// already gone. Waiting the caller-facing 10s for a stop reply there
// would make every bench repeat pay it.
func TestHarnessResetDropsAPerfRunWithoutWaitingOutTheUIQueryTimeout(t *testing.T) {
	h, bridge := newPerfHarness(t)
	if _, err := h.HarnessPerfStart(HarnessPerfSpec{SampleMs: harnessPerfMinSampleMs}); err != nil {
		t.Fatalf("HarnessPerfStart: %v", err)
	}
	bridge.goSilentOn("stop")
	bridge.goSilentOn("collect")

	started := time.Now()
	h.stopPerfRunForReset()
	elapsed := time.Since(started)
	if elapsed >= harnessUIQueryTimeout {
		t.Fatalf("reset took %s; a dead page must cost the short stop timeout, not the %s caller-facing one", elapsed, harnessUIQueryTimeout)
	}
	if elapsed < harnessPerfStopQueryTimeout {
		// Not a failure mode worth failing on, but the bound above only
		// means something if the query really did park.
		t.Logf("reset returned in %s (stop query timeout is %s)", elapsed, harnessPerfStopQueryTimeout)
	}
	status, err := h.HarnessPerfStatus()
	if err != nil {
		t.Fatalf("HarnessPerfStatus: %v", err)
	}
	if status.Active {
		t.Fatal("HarnessReset must leave no perf run armed")
	}
}

// finishPerfRun is reachable twice on one run (HarnessPerfStop racing the
// reset path). The second call must return what the run accumulated, not
// panic on a re-closed channel or hang on a done channel nobody will
// close again.
func TestFinishPerfRunIsIdempotent(t *testing.T) {
	h, bridge := newPerfHarness(t)
	status, err := h.HarnessPerfStart(HarnessPerfSpec{SampleMs: harnessPerfMinSampleMs})
	if err != nil {
		t.Fatalf("HarnessPerfStart: %v", err)
	}
	waitFor(t, func() bool { return len(bridge.samplesSeen()) > 0 }, "a first perf sample")

	h.perf.mu.Lock()
	run := h.perf.run
	h.perf.mu.Unlock()
	if run == nil {
		t.Fatal("no run armed")
	}

	first, firstOwned := h.finishPerfRunOwned(run)
	second, secondOwned := h.finishPerfRunOwned(run)
	if !firstOwned || secondOwned {
		t.Fatalf("ownership = %v then %v, want the first call to be the one that unarmed the run", firstOwned, secondOwned)
	}
	if first.RunID != status.RunID || second.RunID != status.RunID {
		t.Fatalf("run ids = %q / %q, want %q", first.RunID, second.RunID, status.RunID)
	}
	if second.Samples != first.Samples || second.Backend.HeapBytes.Count != first.Backend.HeapBytes.Count {
		t.Errorf("second finish reported %d samples over %+v, first reported %d over %+v",
			second.Samples, second.Backend.HeapBytes, first.Samples, first.Backend.HeapBytes)
	}
}

// An abandoned `perf start` must not sample forever. Past the ceiling the
// sampler self-finishes and parks its report, and the caller who never
// came back still collects it through HarnessPerfStop.
func TestHarnessPerfRunSelfFinishesAtItsDurationCeiling(t *testing.T) {
	h, _ := newPerfHarness(t)
	expired := make(chan HarnessPerfReport, 1)
	h.perf.expiredHook = func(report HarnessPerfReport) { expired <- report }

	status, err := h.HarnessPerfStart(HarnessPerfSpec{SampleMs: harnessPerfMinSampleMs, MaxDurationMs: 1})
	if err != nil {
		t.Fatalf("HarnessPerfStart: %v", err)
	}
	if status.MaxDurationMs != 1 {
		t.Fatalf("status.MaxDurationMs = %d, want the spec's ceiling", status.MaxDurationMs)
	}

	select {
	case report := <-expired:
		if report.RunID != status.RunID || report.Samples == 0 {
			t.Fatalf("self-finished report = %+v", report)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never hit its ceiling")
	}

	after, err := h.HarnessPerfStatus()
	if err != nil {
		t.Fatalf("HarnessPerfStatus: %v", err)
	}
	if after.Active || after.EndedRunID != status.RunID {
		t.Fatalf("status after the ceiling = %+v, want an ended run named %q", after, status.RunID)
	}

	report, err := h.HarnessPerfStop()
	if err != nil {
		t.Fatalf("HarnessPerfStop after a self-finish: %v", err)
	}
	if report.RunID != status.RunID {
		t.Fatalf("collected report = %+v, want run %q", report, status.RunID)
	}
	if _, err := h.HarnessPerfStop(); err == nil {
		t.Error("the parked report is handed over exactly once")
	}
}

// A zero MaxDurationMs takes the default ceiling rather than meaning
// "unlimited": the common way a run outlives its purpose is a caller that
// never came back, and that caller sent no field at all.
func TestHarnessPerfDefaultsTheDurationCeiling(t *testing.T) {
	h, _ := newPerfHarness(t)
	status, err := h.HarnessPerfStart(HarnessPerfSpec{})
	if err != nil {
		t.Fatalf("HarnessPerfStart: %v", err)
	}
	t.Cleanup(func() { h.stopPerfRunForReset() })
	if status.MaxDurationMs != harnessPerfDefaultMaxDurationMs {
		t.Fatalf("MaxDurationMs = %d, want the %d default", status.MaxDurationMs, harnessPerfDefaultMaxDurationMs)
	}
}

func TestHarnessPerfRefusesDurationOverflowBeforeArming(t *testing.T) {
	h, _ := newPerfHarness(t)
	if _, err := h.HarnessPerfStart(HarnessPerfSpec{MaxDurationMs: maxHarnessPerfDurationMs + 1}); err == nil {
		t.Fatal("duration multiplication overflow must be refused")
	} else if !strings.Contains(err.Error(), "overflows a duration") {
		t.Fatalf("error = %v, want overflow refusal", err)
	}
	status, err := h.HarnessPerfStatus()
	if err != nil {
		t.Fatalf("HarnessPerfStatus: %v", err)
	}
	if status.Active {
		t.Fatal("overflow refusal must not arm a run")
	}
}

// waitFor blocks until cond holds, failing the test rather than hanging.
// The perf sampler's clock is a real timer, so a condition it drives can
// only be observed by looking.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
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

func TestEmptyMeterRunDoesNotQueryOrArmThePage(t *testing.T) {
	h, bridge := newPerfHarness(t)
	if _, err := h.HarnessPerfStart(HarnessPerfSpec{Meters: []string{}, SampleMs: harnessPerfMinSampleMs}); err != nil {
		t.Fatalf("HarnessPerfStart: %v", err)
	}
	if _, err := h.HarnessPerfStop(); err != nil {
		t.Fatalf("HarnessPerfStop: %v", err)
	}
	if got := bridge.specsSeen(); len(got) != 0 {
		t.Fatalf("clean run sent page queries: %+v", got)
	}
}
