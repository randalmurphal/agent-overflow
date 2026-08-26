package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The bench arithmetic is the part a wrong answer would be believed: a
// report is read weeks later by someone who cannot re-run the workload.
// Nothing here spawns anything; every test hands the functions decoded
// documents.

func benchTestReport(durationMs int64, fps, p95 float64, goHeap uint64) perfReport {
	return perfReport{
		RunID:      "run",
		SampleMs:   1000,
		DurationMs: durationMs,
		Samples:    4,
		Frontend: &perfFrontendSummary{
			V: 1,
			Frames: perfFrameSummary{
				Frames: 240, FPS: fps, P50Ms: 8, P95Ms: p95, P99Ms: p95 + 4, MaxMs: p95 + 10,
				LongFrames: 3, LongFrameMs: 50,
			},
			LongTasks:     2,
			LongestTaskMs: 120,
			LayoutShift:   0.0125,
			DomNodes:      perfSeries{Count: 4, Min: 900, Max: 1400, Mean: 1100, Last: 1400},
			HeapBytes:     perfSeries{Count: 4, Min: 1 << 20, Max: 4 << 20, Mean: 2 << 20, Last: 4 << 20},
			Samples:       4,
		},
		Backend: perfBackendReport{
			HeapBytes:  perfSeries{Count: 4, Min: 1 << 20, Max: float64(goHeap), Mean: 2 << 20},
			Goroutines: perfSeries{Count: 4, Min: 40, Max: 61, Mean: 50},
		},
	}
}

func TestAggregateBenchMetricsUsesNearestRank(t *testing.T) {
	reports := []perfReport{
		benchTestReport(1000, 58, 12, 8<<20),
		benchTestReport(2000, 60, 20, 12<<20),
		benchTestReport(3000, 55, 16, 10<<20),
	}
	agg := aggregateBenchMetrics(reports)

	duration, ok := agg["duration.ms"]
	if !ok {
		t.Fatal("duration.ms missing from the aggregate")
	}
	if duration.Runs != 3 {
		t.Errorf("Runs = %d, want 3", duration.Runs)
	}
	// Sorted [1000 2000 3000]: nearest-rank p50 is ceil(0.5*3)=2 -> 2000,
	// and p95 is ceil(0.95*3)=3 -> 3000. Neither invents a value.
	if duration.P50 != 2000 || duration.P95 != 3000 {
		t.Errorf("p50/p95 = %v/%v, want 2000/3000", duration.P50, duration.P95)
	}
	if duration.Min != 1000 || duration.Max != 3000 {
		t.Errorf("min/max = %v/%v, want 1000/3000", duration.Min, duration.Max)
	}
	if !duration.LowerIsBetter || duration.Unit != "ms" {
		t.Errorf("duration.ms should be a lower-is-better ms metric, got %+v", duration)
	}

	fps := agg["frames.fps"]
	if fps.LowerIsBetter {
		t.Error("frames.fps must be higher-is-better")
	}
	if fps.P50 != 58 {
		t.Errorf("fps p50 = %v, want 58 (sorted [55 58 60], rank 2)", fps.P50)
	}
}

func TestAggregateBenchMetricsOmitsUnmeasured(t *testing.T) {
	// A run whose page never answered has no frontend half. Folding that as
	// zero would halve the mean and make a broken run look fast.
	headless := benchTestReport(1000, 60, 12, 8<<20)
	headless.Frontend = nil
	headless.FrontendError = "no frontend attached"

	agg := aggregateBenchMetrics([]perfReport{headless, benchTestReport(1200, 59, 14, 8<<20)})
	if got := agg["frames.p95Ms"].Runs; got != 1 {
		t.Errorf("frames.p95Ms folded %d runs, want 1", got)
	}
	if got := agg["frames.p95Ms"].P50; got != 14 {
		t.Errorf("frames.p95Ms p50 = %v, want 14", got)
	}
	if got := agg["duration.ms"].Runs; got != 2 {
		t.Errorf("duration.ms folded %d runs, want 2 (it needs no page)", got)
	}
	if _, ok := agg["webviewRss.maxBytes"]; ok {
		t.Error("webviewRss.maxBytes should be absent: no sample carried it")
	}
}

func floatPtr(v float64) *float64 { return &v }

func TestCompareToBaselineExplicitBudget(t *testing.T) {
	current := map[string]benchAggregate{
		"frames.p95Ms": {Runs: 3, P50: 24, Unit: "ms", LowerIsBetter: true},
		"frames.fps":   {Runs: 3, P50: 44, Unit: "fps", LowerIsBetter: false},
	}
	baseline := benchBaseline{Metrics: map[string]benchTolerance{
		"frames.p95Ms": {Max: floatPtr(20)},
		"frames.fps":   {Min: floatPtr(50)},
	}}

	comparisons, unmeasured := compareToBaseline(current, baseline)
	if len(unmeasured) != 0 {
		t.Fatalf("unmeasured = %v, want none", unmeasured)
	}
	if len(comparisons) != 2 {
		t.Fatalf("got %d comparisons, want 2", len(comparisons))
	}
	// Sorted by name: frames.fps then frames.p95Ms.
	if comparisons[0].Metric != "frames.fps" || !comparisons[0].Drift {
		t.Errorf("44 fps under a 50 floor must drift: %+v", comparisons[0])
	}
	if comparisons[0].Note != "min" {
		t.Errorf("note = %q, want min", comparisons[0].Note)
	}
	if comparisons[1].Metric != "frames.p95Ms" || !comparisons[1].Drift {
		t.Errorf("24ms over a 20ms ceiling must drift: %+v", comparisons[1])
	}
}

func TestCompareToBaselineFromPreviousReport(t *testing.T) {
	current := map[string]benchAggregate{
		"frames.p95Ms":    {Runs: 3, P50: 12, Unit: "ms", LowerIsBetter: true},
		"goHeap.maxBytes": {Runs: 3, P50: 40 << 20, Unit: "bytes", LowerIsBetter: true},
	}
	// A previous report's own aggregate map, which is what a bench writes.
	baseline := benchBaseline{Aggregate: map[string]benchAggregate{
		"frames.p95Ms":    {Runs: 3, P50: 10, Unit: "ms", LowerIsBetter: true},
		"goHeap.maxBytes": {Runs: 3, P50: 20 << 20, Unit: "bytes", LowerIsBetter: true},
		"longTasks":       {Runs: 3, P50: 4, Unit: "count", LowerIsBetter: true},
	}}

	comparisons, unmeasured := compareToBaseline(current, baseline)
	if len(unmeasured) != 1 || unmeasured[0] != "longTasks" {
		t.Fatalf("unmeasured = %v, want [longTasks]", unmeasured)
	}
	byName := map[string]benchComparison{}
	for _, comparison := range comparisons {
		byName[comparison.Metric] = comparison
	}
	// 12ms against a 10ms reference is +20%, inside the default 25% budget.
	frames := byName["frames.p95Ms"]
	if frames.Drift {
		t.Errorf("12ms vs a 10ms reference is inside the default budget: %+v", frames)
	}
	if frames.Limit != 12.5 {
		t.Errorf("limit = %v, want 12.5", frames.Limit)
	}
	if !strings.Contains(frames.Note, "default") {
		t.Errorf("note = %q, want it to name the default rule", frames.Note)
	}
	// Doubling the heap is well past it.
	if !byName["goHeap.maxBytes"].Drift {
		t.Errorf("a doubled go heap must drift: %+v", byName["goHeap.maxBytes"])
	}
}

func TestCompareToBaselineBudgetBeatsReport(t *testing.T) {
	current := map[string]benchAggregate{
		"frames.p95Ms": {Runs: 1, P50: 30, Unit: "ms", LowerIsBetter: true},
	}
	baseline := benchBaseline{
		Aggregate: map[string]benchAggregate{
			"frames.p95Ms": {Runs: 1, P50: 40, Unit: "ms", LowerIsBetter: true},
		},
		Metrics: map[string]benchTolerance{
			"frames.p95Ms": {Max: floatPtr(25)},
		},
	}
	comparisons, _ := compareToBaseline(current, baseline)
	if len(comparisons) != 1 {
		t.Fatalf("got %d comparisons, want 1", len(comparisons))
	}
	if comparisons[0].Limit != 25 || !comparisons[0].Drift {
		t.Errorf("the written-down budget must win over the report reference: %+v", comparisons[0])
	}
}

func TestCompareToBaselineNoReferenceNeverDrifts(t *testing.T) {
	// A tolerance carrying only a percentage has nothing to apply it to.
	// Reporting that as drift would fail a bench on a malformed file.
	current := map[string]benchAggregate{
		"frames.p95Ms": {Runs: 1, P50: 999, Unit: "ms", LowerIsBetter: true},
	}
	baseline := benchBaseline{Metrics: map[string]benchTolerance{
		"frames.p95Ms": {MaxPctOver: floatPtr(10)},
	}}
	comparisons, _ := compareToBaseline(current, baseline)
	if comparisons[0].Drift {
		t.Errorf("a budget with no reference cannot drift: %+v", comparisons[0])
	}
	if comparisons[0].Note != "no reference" {
		t.Errorf("note = %q, want \"no reference\"", comparisons[0].Note)
	}
}

// TestBenchDocumentShape is the golden: the file a bench writes is the file
// --baseline reads back, so its key names are a contract with future runs.
func TestBenchDocumentShape(t *testing.T) {
	document := benchDocument{
		Workload:    "burst-stream",
		Description: "sustained text-delta flood with chunked partial writes",
		Scenario:    "bench-burst-stream",
		Repeat:      1,
		StartedAt:   "2026-08-26T10:00:00Z",
		Instance:    "abcd1234",
		Version:     "test",
		Runs: []benchRunReport{{
			Run: 1, StartedAt: "2026-08-26T10:00:00Z", DurationMs: 4200,
			Perf: benchTestReport(4200, 58, 14, 8<<20),
		}},
		Aggregate: aggregateBenchMetrics([]perfReport{benchTestReport(4200, 58, 14, 8<<20)}),
	}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"workload", "repeat", "startedAt", "instance", "version", "runs", "aggregate"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("report document is missing %q", key)
		}
	}
	runs, _ := decoded["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", decoded["runs"])
	}
	run, _ := runs[0].(map[string]any)
	if _, ok := run["perf"]; !ok {
		t.Error("a run row must carry its whole perf report")
	}

	// The round trip is the property that matters: yesterday's report has to
	// load as today's baseline without any conversion step.
	var baseline benchBaseline
	if err := json.Unmarshal(body, &baseline); err != nil {
		t.Fatal(err)
	}
	if baseline.Workload != "burst-stream" {
		t.Errorf("baseline workload = %q", baseline.Workload)
	}
	if got := baseline.Aggregate["frames.fps"]; got.P50 != 58 || got.LowerIsBetter {
		t.Errorf("frames.fps did not survive the round trip: %+v", got)
	}
}

func TestFormatBenchValueUnits(t *testing.T) {
	if got := formatBenchValue(1536, "bytes"); got != "2K" {
		t.Errorf("bytes = %q", got)
	}
	if got := formatBenchValue(12.4, "count"); got != "12" {
		t.Errorf("count = %q", got)
	}
	if got := formatBenchValue(0.01234, "score"); got != "0.0123" {
		t.Errorf("score = %q", got)
	}
	if got := formatBenchValue(14.26, "ms"); got != "14.3" {
		t.Errorf("ms = %q", got)
	}
}
