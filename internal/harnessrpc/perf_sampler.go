// app_harness_perf_sampler.go is the clock half of a perf run: the
// goroutine that ticks, the fold of one tick into a `harness:perf` frame,
// and the Go-side numbers that frame carries. The lifecycle around it —
// arming, stopping, the duration ceiling, the report — lives in
// app_harness_perf.go, which is also where the design rationale for one
// backend-owned clock is written down.
package harnessrpc

import (
	"encoding/json"
	"log"
	"os"
	"runtime"
	"runtime/metrics"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/procrss"
)

// runPerfSampler is the one clock. It sleeps a full interval BETWEEN cycles
// rather than firing on a ticker, so a slow collect delays the next sample
// instead of queueing a backlog of them.
func (h *Harness) runPerfSampler(run *harnessPerfRun) {
	defer close(run.done)
	interval := time.Duration(run.sampleMs) * time.Millisecond
	collectTimeout := interval
	if collectTimeout > harnessPerfMaxCollectTimeout {
		collectTimeout = harnessPerfMaxCollectTimeout
	}
	// The run id rides on every outgoing spec so a bridge attached to a
	// DIFFERENT run (two pages on one instance) can decline to answer
	// rather than win the reply race with an error. A bridge that ignores
	// the field behaves exactly as before.
	collect, err := json.Marshal(map[string]any{"v": 1, "kind": "perf", "op": "collect", "runId": run.id, "pageId": run.pageID})
	if err != nil {
		log.Printf("harness: perf: encode collect query: %v", err)
		return
	}
	var deadline time.Time
	if run.maxDuration > 0 {
		deadline = run.startedAt.Add(run.maxDuration)
	}
	for {
		h.emitPerfSample(run, collect, collectTimeout)
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			h.expirePerfRun(run)
			return
		}
		timer := time.NewTimer(interval)
		select {
		case <-run.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (h *Harness) emitPerfSample(run *harnessPerfRun, collect json.RawMessage, timeout time.Duration) {
	backend := sampleHarnessPerfBackend(run.prefixes)
	var frontend json.RawMessage
	var frontendErr error
	if run.frontendEnabled {
		frontend, frontendErr = h.queryUI(collect, timeout)
	}
	if len(run.monitorHeartbeat) > 0 {
		if _, err := h.queryUI(run.monitorHeartbeat, timeout); err != nil {
			h.perf.mu.Lock()
			run.monitorLastError = err.Error()
			h.perf.mu.Unlock()
		}
	}

	event := harnessPerfEvent{
		RunID:   run.id,
		AtMs:    time.Since(run.startedAt).Milliseconds(),
		Backend: backend,
	}
	if frontendErr != nil {
		event.FrontendError = frontendErr.Error()
	} else {
		event.Frontend = frontend
	}

	h.perf.mu.Lock()
	run.seq++
	event.Seq = run.seq
	if frontendErr != nil {
		run.lastErr = frontendErr.Error()
	} else if run.frontendEnabled {
		run.frontendSamples++
		run.lastErr = ""
	}
	run.report.HeapBytes.add(float64(backend.HeapBytes))
	run.report.HeapObjects.add(float64(backend.HeapObjects))
	run.report.Goroutines.add(float64(backend.Goroutines))
	if backend.RSSBytes > 0 {
		run.report.RSSBytes.add(float64(backend.RSSBytes))
		run.report.Processes = backend.Processes
	}
	// Only a tick that actually MATCHED a webview child contributes to the
	// renderer series. Recording a zero whenever the walk found none would
	// report "the renderer used no memory" on every Windows/WSL run, where
	// WebView2 is the launcher's child and never appears in our subtree.
	if len(backend.Processes) > 0 {
		run.report.WebviewRSSBytes.add(float64(backend.ChildrenRSSBytes))
	}
	h.perf.mu.Unlock()

	if h.config.Host != nil {
		h.config.Host.Emit(eventchan.HarnessPerf, event)
	}
}

// harnessPerfMetricNames are read through runtime/metrics rather than
// runtime.ReadMemStats: ReadMemStats stops the world, which would make the
// sampler a source of the very jank the run is measuring.
var harnessPerfMetricNames = []string{
	"/memory/classes/heap/objects:bytes",
	"/gc/heap/objects:objects",
	"/sched/goroutines:goroutines",
}

func sampleHarnessPerfBackend(prefixes []string) harnessPerfBackendSample {
	samples := make([]metrics.Sample, len(harnessPerfMetricNames))
	for i, name := range harnessPerfMetricNames {
		samples[i].Name = name
	}
	metrics.Read(samples)
	sample := harnessPerfBackendSample{}
	for i, s := range samples {
		value := uint64(0)
		if s.Value.Kind() == metrics.KindUint64 {
			value = s.Value.Uint64()
		}
		switch harnessPerfMetricNames[i] {
		case "/memory/classes/heap/objects:bytes":
			sample.HeapBytes = value
		case "/gc/heap/objects:objects":
			sample.HeapObjects = value
		case "/sched/goroutines:goroutines":
			sample.Goroutines = int(value)
		}
	}
	// A runtime that ever stops publishing the scheduler metric must not
	// report zero goroutines — that reads as a dead process.
	if sample.Goroutines == 0 {
		sample.Goroutines = runtime.NumGoroutine()
	}

	if !procrss.Supported() {
		return sample
	}
	tree, err := procrss.Sample(os.Getpid(), prefixes)
	if err != nil {
		// A native process read that fails leaves the series short by one sample
		// rather than failing the run; the RSS series' Count is what says
		// how many landed.
		return sample
	}
	sample.RSSBytes = tree.Self.RSSBytes
	sample.ChildrenRSSBytes = tree.ChildrenRSSBytes
	sample.Processes = tree.Children
	sample.RSSAvailable = true
	sample.WebviewRSSMeasurable = len(tree.Children) > 0
	return sample
}
