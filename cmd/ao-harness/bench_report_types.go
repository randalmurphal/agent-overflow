package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

const benchReportSchemaVersion = 2

type benchReportIdentity struct {
	SchemaVersion       int    `json:"schemaVersion"`
	Leg                 string `json:"leg"`
	Instrument          string `json:"instrument"`
	PageID              string `json:"pageId,omitempty"`
	BuildFingerprint    string `json:"buildFingerprint"`
	AssetsFingerprint   string `json:"assetsFingerprint"`
	TimingFingerprint   string `json:"timingFingerprint"`
	BudgetFingerprint   string `json:"budgetFingerprint"`
	MonitorFingerprint  string `json:"monitorFingerprint"`
	TraceFingerprint    string `json:"traceFingerprint"`
	WorkloadFingerprint string `json:"workloadFingerprint"`
}

func fingerprintBenchParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		// Length framing prevents ["ab", "c"] and ["a", "bc"] from
		// sharing an identity while keeping the representation stable.
		fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

// The shapes a bench report is built from, and the arithmetic over them.
// Split from the runner so the maths is testable without a backend: every
// function here takes decoded documents and returns decoded documents.

// perfSeries is the fold both halves of a perf report use. The Go side
// omits `sum`; the bridge carries it. Decoding one shape for both is
// deliberate: a reader wants min/max/mean and does not care which
// process computed them.
type perfSeries struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
	Last  float64 `json:"last"`
}

type perfFrameSummary struct {
	Frames      int     `json:"frames"`
	ElapsedMs   float64 `json:"elapsedMs"`
	FPS         float64 `json:"fps"`
	P50Ms       float64 `json:"p50Ms"`
	P95Ms       float64 `json:"p95Ms"`
	P99Ms       float64 `json:"p99Ms"`
	MaxMs       float64 `json:"maxMs"`
	MeanMs      float64 `json:"meanMs"`
	LongFrames  int     `json:"longFrames"`
	LongFrameMs float64 `json:"longFrameMs"`
}

// perfBusyBudget is one budget's fit. `withinPct` is the number a gate is
// written against: the share of measured ticks whose main-thread busy time
// fit inside `budgetMs`.
type perfBusyBudget struct {
	BudgetMs    float64 `json:"budgetMs"`
	WithinTicks int     `json:"withinTicks"`
	WithinPct   float64 `json:"withinPct"`
}

// perfBusyWorstTick is one of the run's worst main-thread ticks: how long
// it cost, and when it started on the page clock (`performance.now()` at
// the tick's rAF-callback entry). Pair `atMs` with the summary's
// `timeOriginMs` to place it against a trace or a wall-clock log.
type perfBusyWorstTick struct {
	AtMs   float64 `json:"atMs"`
	BusyMs float64 `json:"busyMs"`
}

// perfBusySummary is the whole-run busy-time fold. `ticks` is the count of
// MEASURED ticks — zero means this engine never armed the meter, which is
// not the same claim as "every tick fit", so every reader here gates on it.
type perfBusySummary struct {
	Ticks   int              `json:"ticks"`
	Dropped int              `json:"dropped"`
	P50Ms   float64          `json:"p50Ms"`
	P95Ms   float64          `json:"p95Ms"`
	MaxMs   float64          `json:"maxMs"`
	MeanMs  float64          `json:"meanMs"`
	Budgets []perfBusyBudget `json:"budgets"`
	// Worst is the run's worst ticks, descending. It rides the report and
	// the per-repeat rows of a bench document, and is deliberately NOT a
	// metric: a baseline compares numbers, and a list of timestamps is
	// EVIDENCE — the same rule the forced-layout call-site ranking follows.
	Worst []perfBusyWorstTick `json:"worst,omitempty"`
}

type perfFrontendSummary struct {
	V          int     `json:"v"`
	DurationMs float64 `json:"durationMs"`
	// TimeOriginMs is the document's performance.timeOrigin in epoch
	// milliseconds — the one number that turns every page-clock time in
	// this document (the busy meter's worst ticks) into a wall clock.
	TimeOriginMs            float64          `json:"timeOriginMs,omitempty"`
	Meters                  []string         `json:"meters"`
	UnavailableMeters       []string         `json:"unavailableMeters"`
	Frames                  perfFrameSummary `json:"frames"`
	Busy                    perfBusySummary  `json:"busy"`
	LongTasks               int              `json:"longTasks"`
	LongestTaskMs           float64          `json:"longestTaskMs"`
	LongAnimationFrames     int              `json:"longAnimationFrames"`
	LongestAnimationFrameMs float64          `json:"longestAnimationFrameMs"`
	LayoutShift             float64          `json:"layoutShift"`
	SlowEvents              int              `json:"slowEvents"`
	WorstEventLatencyMs     float64          `json:"worstEventLatencyMs"`
	DomNodes                perfSeries       `json:"domNodes"`
	HeapBytes               perfSeries       `json:"heapBytes"`
	Samples                 int              `json:"samples"`
}

type perfBackendReport struct {
	HeapBytes       perfSeries `json:"heapBytes"`
	Goroutines      perfSeries `json:"goroutines"`
	RSSBytes        perfSeries `json:"rssBytes"`
	WebviewRSSBytes perfSeries `json:"webviewRssBytes"`
}

type perfReport struct {
	RunID         string               `json:"runId"`
	SampleMs      int                  `json:"sampleMs"`
	DurationMs    int64                `json:"durationMs"`
	Samples       int                  `json:"samples"`
	Frontend      *perfFrontendSummary `json:"frontend"`
	FrontendError string               `json:"frontendError"`
	Backend       perfBackendReport    `json:"backend"`
	Monitors      json.RawMessage      `json:"monitors"`
	MonitorsError string               `json:"monitorsError"`
}

func decodePerfReport(raw json.RawMessage) (perfReport, error) {
	var out perfReport
	if err := json.Unmarshal(raw, &out); err != nil {
		return perfReport{}, fmt.Errorf("decode perf report: %w", err)
	}
	return out, nil
}
