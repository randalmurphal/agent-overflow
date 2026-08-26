// app_harness_perf.go turns frames per second, heap and RSS into things a
// test can assert on (docs/specs/testing-harness.md §5). It owns a run's
// LIFECYCLE — arming, stopping, the duration ceiling, the report; the
// clock that fills one lives in app_harness_perf_sampler.go.
//
// One design decision drives both files: THE BACKEND OWNS THE CADENCE.
// The in-page meters accumulate continuously (rAF deltas, PerformanceObserver
// entries), but they never push — each tick the sampler issues a
// `perf/collect` ui-query, reads the frontend's instantaneous sample, folds
// it together with the Go-side numbers, and emits ONE `harness:perf` event.
// A bridge that pushed on its own timer would give the run two clocks: the
// two halves would interleave out of order on the wire, a reader would have
// to re-pair them by timestamp, and a frontend that went away would simply
// stop appearing rather than showing up as a labelled gap. With one clock,
// sample N is one frame with both halves or one frame that says which half
// is missing, and `ao-harness perf watch` is a `tail`.
//
// The frontend summary is the BRIDGE's, computed over the whole run: frame
// percentiles need the full distribution, and shipping every frame delta over
// the wire to recompute them backend-side would cost more than the meters do.
// So the tick carries instantaneous numbers and the stop carries the summary.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/procrss"
)

const (
	// harnessPerfDefaultSampleMs is one sample a second: fine enough to see
	// a turn's shape, coarse enough that the sampling is not the load.
	harnessPerfDefaultSampleMs = 1000
	// harnessPerfMinSampleMs floors the interval. Below this the collect
	// round-trip is a meaningful share of the interval and the run measures
	// itself.
	harnessPerfMinSampleMs = 250
	// harnessPerfMaxCollectTimeout bounds one tick's ui-query. A collect
	// that cannot answer inside the sample interval is a SKIPPED sample
	// (recorded as a frontend error on that frame), never a stalled run.
	harnessPerfMaxCollectTimeout = 2 * time.Second
	// harnessPerfStopQueryTimeout bounds the page-side `perf/stop` query.
	// Deliberately far below harnessUIQueryTimeout: finishPerfRun also runs
	// on the RESET path, where the page is usually already gone, and a
	// bench repeat that paid the caller-facing 10s per reset would spend
	// most of its wall clock waiting for a reply that is never coming. The
	// summary is one already-accumulated object the bridge returns
	// immediately or not at all — a longer wait buys nothing.
	harnessPerfStopQueryTimeout = 2 * time.Second
	// harnessPerfDefaultMaxDurationMs is the run's duration ceiling. An
	// `ao-harness perf start` whose caller wandered off would otherwise
	// sample (and hold an armed page meter) forever; past this the sampler
	// self-finishes and parks the report for collection.
	harnessPerfDefaultMaxDurationMs = 30 * 60 * 1000
)

// HarnessPerfSpec is what a caller arms a run with. Everything is optional;
// the zero value is a 1s run with default meters and the default long-frame
// threshold.
type HarnessPerfSpec struct {
	SampleMs int `json:"sampleMs,omitempty"`
	// LongFrameMs is the frame-time above which a frame counts as long.
	// Forwarded to the bridge verbatim; 0 means the bridge's default (50ms).
	LongFrameMs int `json:"longFrameMs,omitempty"`
	// Meters narrows which in-page meters arm. Empty means all of them.
	// Forwarded verbatim — the vocabulary is the bridge's.
	Meters []string `json:"meters,omitempty"`
	// BudgetsMs are the main-thread budgets the busy meter reports fit
	// against, in milliseconds. Forwarded verbatim; empty means the
	// bridge's default (6, 8, 16). Unlike Meters an unrecognised value is
	// not a refusal — the bridge cleans the list — because a bad budget
	// narrows nothing, it just would not have been reported.
	BudgetsMs []float64 `json:"budgetsMs,omitempty"`
	// WebviewPrefixes overrides which child process names count as webview
	// processes in the /proc walk. Empty means procrss.DefaultWebviewPrefixes.
	WebviewPrefixes []string `json:"webviewPrefixes,omitempty"`
	// MaxDurationMs caps how long the run samples before it self-finishes.
	// Zero means harnessPerfDefaultMaxDurationMs — NOT unlimited, because
	// the common way a run outlives its purpose is a caller that never
	// came back. Negative means no ceiling, for an hours-long soak that
	// deliberately wants one.
	MaxDurationMs int64 `json:"maxDurationMs,omitempty"`
}

// HarnessPerfStatusResult answers "is a run armed, and how is it doing".
type HarnessPerfStatusResult struct {
	Active bool   `json:"active"`
	RunID  string `json:"runId,omitempty"`
	// SampleMs is the resolved interval, after clamping.
	SampleMs int `json:"sampleMs,omitempty"`
	// MaxDurationMs is the resolved ceiling, after defaulting. Zero means
	// the run has none.
	MaxDurationMs int64 `json:"maxDurationMs,omitempty"`
	// Samples counts the ticks EMITTED so far.
	Samples int `json:"samples"`
	// FrontendSamples counts the ticks whose collect query answered.
	FrontendSamples int    `json:"frontendSamples"`
	ElapsedMs       int64  `json:"elapsedMs,omitempty"`
	LastError       string `json:"lastError,omitempty"`
	// RSSAvailable reports whether this platform has a /proc to walk.
	RSSAvailable bool `json:"rssAvailable"`
	// WebviewRSSMeasurable reports whether any sample so far MATCHED a
	// webview child process. False is the norm on the Windows/WSL
	// topology, where WebView2 is the launcher's child rather than ours:
	// the renderer's memory is real but unreachable from this process's
	// /proc subtree, and a reader must be able to tell that from "the
	// renderer used nothing".
	WebviewRSSMeasurable bool `json:"webviewRssMeasurable"`
	// EndedRunID names a run the duration ceiling self-finished whose
	// report nobody has collected yet. The next HarnessPerfStop returns it.
	EndedRunID string `json:"endedRunId,omitempty"`
}

// harnessPerfBackendSample is the Go-side half of one tick.
type harnessPerfBackendSample struct {
	HeapBytes   uint64 `json:"heapBytes"`
	HeapObjects uint64 `json:"heapObjects"`
	Goroutines  int    `json:"goroutines"`
	// RSSBytes / Processes are absent off linux, and Processes is empty for
	// a headless harness (there is no webview child to find).
	RSSBytes         uint64            `json:"rssBytes,omitempty"`
	ChildrenRSSBytes uint64            `json:"childrenRssBytes,omitempty"`
	Processes        []procrss.Process `json:"processes,omitempty"`
}

// harnessPerfEvent is the `harness:perf` wire shape: one folded sample.
type harnessPerfEvent struct {
	RunID   string                   `json:"runId"`
	Seq     int                      `json:"seq"`
	AtMs    int64                    `json:"atMs"`
	Backend harnessPerfBackendSample `json:"backend"`
	// Frontend is the bridge's instantaneous sample, forwarded verbatim.
	// Absent when the collect query did not answer — FrontendError says why,
	// so a gap in the series is legible rather than invisible.
	Frontend      json.RawMessage `json:"frontend,omitempty"`
	FrontendError string          `json:"frontendError,omitempty"`
}

// harnessPerfSeries accumulates one numeric series without retaining it.
// A run is unbounded in length; the summary is not.
type harnessPerfSeries struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
	Last  float64 `json:"last"`

	sum float64
}

func (s *harnessPerfSeries) add(value float64) {
	if s.Count == 0 || value < s.Min {
		s.Min = value
	}
	if s.Count == 0 || value > s.Max {
		s.Max = value
	}
	s.Count++
	s.sum += value
	s.Last = value
	s.Mean = s.sum / float64(s.Count)
}

// harnessPerfBackendReport folds every backend sample of a run.
type harnessPerfBackendReport struct {
	HeapBytes   harnessPerfSeries `json:"heapBytes"`
	HeapObjects harnessPerfSeries `json:"heapObjects"`
	Goroutines  harnessPerfSeries `json:"goroutines"`
	// RSSBytes covers the backend process. Count 0 means no sample landed:
	// the platform has no /proc, or every read of it failed.
	RSSBytes harnessPerfSeries `json:"rssBytes"`
	// WebviewRSSBytes covers the matched children summed, and takes a
	// sample only on a tick that MATCHED at least one. Count 0 therefore
	// means "the renderer was not measurable from here" — the normal
	// answer on Windows/WSL, where WebView2 belongs to the launcher rather
	// than to this process — and never "the renderer used no memory". A
	// zero-valued sample would have been indistinguishable from the latter.
	WebviewRSSBytes harnessPerfSeries `json:"webviewRssBytes"`
	// Processes is the LAST tick's per-process breakdown, which is what a
	// "what is holding it" question wants; the series above carry the shape
	// over time.
	Processes []procrss.Process `json:"processes,omitempty"`
}

// HarnessPerfReport is what HarnessPerfStop returns.
type HarnessPerfReport struct {
	RunID      string `json:"runId"`
	SampleMs   int    `json:"sampleMs"`
	DurationMs int64  `json:"durationMs"`
	Samples    int    `json:"samples"`
	// Frontend is the bridge's whole-run summary (frame histogram, observer
	// counts, heap/DOM), forwarded verbatim. Absent when no frontend
	// answered the stop query; FrontendError then says why.
	Frontend      json.RawMessage          `json:"frontend,omitempty"`
	FrontendError string                   `json:"frontendError,omitempty"`
	Backend       harnessPerfBackendReport `json:"backend"`
}

// harnessPerfState is the one armed run. Its own mutex: the sampler
// goroutine touches it every tick.
type harnessPerfState struct {
	mu     sync.Mutex
	run    *harnessPerfRun
	nextID uint64
	// expired parks the report of a run the DURATION CEILING finished, so
	// the caller who never got to call HarnessPerfStop can still collect
	// it. At most one is held: the HarnessPerfStop that returns it clears
	// it, and so does the next HarnessPerfStart — this is a handoff slot,
	// not a history.
	expired *HarnessPerfReport
	// expiredHook fires after a self-finished report has been parked. Nil
	// in production; a test installs one to synchronise on the ceiling
	// rather than poll for it.
	expiredHook func(HarnessPerfReport)
}

type harnessPerfRun struct {
	id          string
	sampleMs    int
	maxDuration time.Duration
	prefixes    []string
	startedAt   time.Time
	stop        chan struct{}
	done        chan struct{}

	seq             int
	frontendSamples int
	lastErr         string
	report          harnessPerfBackendReport
}

// HarnessPerfStart arms the in-page meters and begins backend sampling.
//
// The arm goes through the same ui-query mechanism everything else does, so
// "no frontend attached" fails HERE — loudly, at the call that asked for
// meters — rather than producing a run of half-empty samples.
func (h *Harness) HarnessPerfStart(spec HarnessPerfSpec) (HarnessPerfStatusResult, error) {
	sampleMs := spec.SampleMs
	if sampleMs <= 0 {
		sampleMs = harnessPerfDefaultSampleMs
	}
	if sampleMs < harnessPerfMinSampleMs {
		sampleMs = harnessPerfMinSampleMs
	}
	prefixes := spec.WebviewPrefixes
	if len(prefixes) == 0 {
		prefixes = procrss.DefaultWebviewPrefixes
	}
	maxDuration := time.Duration(spec.MaxDurationMs) * time.Millisecond
	switch {
	case spec.MaxDurationMs == 0:
		maxDuration = harnessPerfDefaultMaxDurationMs * time.Millisecond
	case spec.MaxDurationMs < 0:
		maxDuration = 0
	}

	h.perf.mu.Lock()
	if h.perf.run != nil {
		active := h.perf.run.id
		h.perf.mu.Unlock()
		return HarnessPerfStatusResult{}, fmt.Errorf("perf run %s is already active; stop it first", active)
	}
	h.perf.nextID++
	run := &harnessPerfRun{
		id:          fmt.Sprintf("perf-%d", h.perf.nextID),
		sampleMs:    sampleMs,
		maxDuration: maxDuration,
		prefixes:    prefixes,
		startedAt:   time.Now(),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	h.perf.run = run
	// A new run supersedes an uncollected self-finished one: keeping it
	// would hand the NEXT stop a report from a run two arms ago.
	h.perf.expired = nil
	h.perf.mu.Unlock()

	arm, err := json.Marshal(map[string]any{
		"v":           1,
		"kind":        "perf",
		"op":          "start",
		"runId":       run.id,
		"longFrameMs": spec.LongFrameMs,
		"meters":      spec.Meters,
		"budgetsMs":   spec.BudgetsMs,
	})
	if err != nil {
		h.clearPerfRun(run)
		return HarnessPerfStatusResult{}, err
	}
	if _, err := h.queryUI(arm, harnessUIQueryTimeout); err != nil {
		h.clearPerfRun(run)
		return HarnessPerfStatusResult{}, fmt.Errorf("arm frontend meters: %w", err)
	}

	go h.runPerfSampler(run)
	return h.HarnessPerfStatus()
}

// HarnessPerfStop stops the sampler, tells the bridge to stop, and returns
// the combined report.
//
// With no run armed it also collects a run the duration ceiling already
// self-finished, so `perf start` / walk away / `perf stop` still returns
// numbers rather than an error about a run that sampled for half an hour.
func (h *Harness) HarnessPerfStop() (HarnessPerfReport, error) {
	h.perf.mu.Lock()
	run := h.perf.run
	if run == nil {
		expired := h.perf.expired
		h.perf.expired = nil
		h.perf.mu.Unlock()
		if expired != nil {
			return *expired, nil
		}
		return HarnessPerfReport{}, fmt.Errorf("no perf run is active")
	}
	h.perf.mu.Unlock()
	return h.finishPerfRun(run), nil
}

// HarnessPerfStatus reports the armed run, or `{active:false}`.
func (h *Harness) HarnessPerfStatus() (HarnessPerfStatusResult, error) {
	h.perf.mu.Lock()
	defer h.perf.mu.Unlock()
	result := HarnessPerfStatusResult{RSSAvailable: procrss.Supported()}
	run := h.perf.run
	if run == nil {
		if h.perf.expired != nil {
			result.EndedRunID = h.perf.expired.RunID
			result.Samples = h.perf.expired.Samples
			result.WebviewRSSMeasurable = h.perf.expired.Backend.WebviewRSSBytes.Count > 0
		}
		return result, nil
	}
	result.Active = true
	result.RunID = run.id
	result.SampleMs = run.sampleMs
	result.MaxDurationMs = run.maxDuration.Milliseconds()
	result.Samples = run.seq
	result.FrontendSamples = run.frontendSamples
	result.ElapsedMs = time.Since(run.startedAt).Milliseconds()
	result.LastError = run.lastErr
	result.WebviewRSSMeasurable = run.report.WebviewRSSBytes.Count > 0
	return result, nil
}

// stopPerfRunForReset drops any armed run without collecting a report.
// HarnessReset's contract is "harness-owned state does not survive", and a
// perf run is a goroutine plus a set of armed in-page meters — the next
// spec must not inherit either. The bridge is told to stop on a best-effort
// basis: the page is about to be reloaded anyway, and a reset must not fail
// because no frontend was attached.
func (h *Harness) stopPerfRunForReset() {
	h.perf.mu.Lock()
	run := h.perf.run
	h.perf.mu.Unlock()
	if run == nil {
		return
	}
	report := h.finishPerfRun(run)
	log.Printf("harness: reset: stopped perf run %s after %d samples", report.RunID, report.Samples)
}

// finishPerfRun stops the sampler goroutine, asks the bridge for its
// summary, and folds the two halves. Safe to call twice on one run: the
// second caller finds the run already cleared and gets what it accumulated.
func (h *Harness) finishPerfRun(run *harnessPerfRun) HarnessPerfReport {
	report, _ := h.finishPerfRunOwned(run)
	return report
}

// finishPerfRunOwned is finishPerfRun plus the bit that says whether THIS
// call was the one that unarmed the run. The self-finish path needs it:
// only the winner of a race with a concurrent HarnessPerfStop may park
// the report, or a caller who already collected it would be handed it
// again by the next stop.
func (h *Harness) finishPerfRunOwned(run *harnessPerfRun) (HarnessPerfReport, bool) {
	h.perf.mu.Lock()
	owned := h.perf.run == run
	if owned {
		h.perf.run = nil
		close(run.stop)
	}
	h.perf.mu.Unlock()
	<-run.done

	report := HarnessPerfReport{
		RunID:      run.id,
		SampleMs:   run.sampleMs,
		DurationMs: time.Since(run.startedAt).Milliseconds(),
	}
	stop, err := json.Marshal(map[string]any{"v": 1, "kind": "perf", "op": "stop", "runId": run.id})
	if err == nil {
		summary, queryErr := h.queryUI(stop, harnessPerfStopQueryTimeout)
		if queryErr != nil {
			report.FrontendError = queryErr.Error()
		} else {
			report.Frontend = summary
		}
	} else {
		report.FrontendError = err.Error()
	}

	h.perf.mu.Lock()
	report.Samples = run.seq
	report.Backend = run.report
	h.perf.mu.Unlock()
	return report, owned
}

// expirePerfRun self-finishes a run that outlived its ceiling and parks
// the report for a later HarnessPerfStop.
//
// It runs on its OWN goroutine because finishPerfRun waits on run.done,
// and run.done is closed by the sampler goroutine calling this — waiting
// for yourself never returns.
func (h *Harness) expirePerfRun(run *harnessPerfRun) {
	go func() {
		report, owned := h.finishPerfRunOwned(run)
		if !owned {
			// A concurrent HarnessPerfStop got there first and holds the
			// report; parking a second copy would double-deliver it.
			return
		}
		h.perf.mu.Lock()
		h.perf.expired = &report
		hook := h.perf.expiredHook
		h.perf.mu.Unlock()
		log.Printf(
			"harness: perf: run %s hit its %s duration ceiling after %d samples and self-finished; collect the report with `ao-harness perf stop`",
			run.id, run.maxDuration, report.Samples,
		)
		if hook != nil {
			hook(report)
		}
	}()
}

// clearPerfRun unwinds a run that never started its sampler.
func (h *Harness) clearPerfRun(run *harnessPerfRun) {
	h.perf.mu.Lock()
	if h.perf.run == run {
		h.perf.run = nil
	}
	h.perf.mu.Unlock()
	close(run.done)
}
