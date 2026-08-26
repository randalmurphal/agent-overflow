package main

import (
	"encoding/json"
	"slices"
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

	// A page that answered but whose series meters never sampled is the
	// other half of the same rule, and the one a series-shaped metric read
	// through frontendMetric would get wrong: the summary is present, so
	// the run looks measured, and a zero would fold in as a real reading.
	unmetered := benchTestReport(1000, 60, 12, 8<<20)
	unmetered.Frontend.DomNodes = perfSeries{}
	unmetered.Frontend.HeapBytes = perfSeries{}
	agg = aggregateBenchMetrics([]perfReport{unmetered, benchTestReport(1200, 59, 14, 8<<20)})
	for _, name := range []string{"domNodes.max", "jsHeap.maxBytes"} {
		if got := agg[name].Runs; got != 1 {
			t.Errorf("%s folded %d runs, want 1 (one run never sampled it)", name, got)
		}
		if agg[name].Min == 0 {
			t.Errorf("%s folded an unsampled run in as zero: %+v", name, agg[name])
		}
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

// A budget of zero is the strictest thing the file can say, not an absent
// one. Reading it as "no opinion" would silently accept every value.
func TestCompareToBaselineHonoursAnExplicitZeroBudget(t *testing.T) {
	current := map[string]benchAggregate{
		"frames.long": {Runs: 1, P50: 3, Unit: "count", LowerIsBetter: true},
		"clean.long":  {Runs: 1, P50: 0, Unit: "count", LowerIsBetter: true},
		"frames.fps":  {Runs: 1, P50: 0, Unit: "fps", LowerIsBetter: false},
	}
	baseline := benchBaseline{Metrics: map[string]benchTolerance{
		"frames.long": {Max: floatPtr(0)},
		"clean.long":  {Max: floatPtr(0)},
		"frames.fps":  {Min: floatPtr(0)},
	}}
	byName := map[string]benchComparison{}
	comparisons, _ := compareToBaseline(current, baseline)
	for _, comparison := range comparisons {
		byName[comparison.Metric] = comparison
	}
	if got := byName["frames.long"]; !got.Drift || got.Note != "max" {
		t.Errorf("3 long frames against a `max: 0` budget must drift: %+v", got)
	}
	if got := byName["clean.long"]; got.Drift {
		t.Errorf("0 long frames meets a `max: 0` budget: %+v", got)
	}
	// A zero floor on a higher-is-better metric is satisfied, not drifted:
	// the rule is resolved and the value is not below it.
	if got := byName["frames.fps"]; got.Drift || got.Note != "min" {
		t.Errorf("0 fps against a `min: 0` floor must not drift: %+v", got)
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

// A gate that could not READ the number it was told to gate is bad news,
// not a pass. The case that forced this: a headless run answers no
// frontend metric, so the comparison table came back empty and `bench
// --baseline` exited 0 — a run that measured nothing reporting success.
func TestABudgetedMetricThatWasNeverMeasuredFailsTheGate(t *testing.T) {
	current := map[string]benchAggregate{
		"goHeap.maxBytes": {Runs: 1, P50: 10 << 20, Unit: "bytes", LowerIsBetter: true},
	}
	baseline := benchBaseline{Metrics: map[string]benchTolerance{
		"goHeap.maxBytes": {Max: floatPtr(64 << 20)},
		"frames.p95Ms":    {Max: floatPtr(20)},
	}}

	comparisons, unmeasured, unbudgeted := compareToBaselineDetailed(current, baseline)
	if len(unbudgeted) != 1 || unbudgeted[0] != "frames.p95Ms" {
		t.Fatalf("unbudgeted = %v, want the explicitly budgeted metric that was not measured", unbudgeted)
	}
	if !slices.Contains(unmeasured, "frames.p95Ms") {
		t.Errorf("unmeasured = %v, want it to carry the same metric", unmeasured)
	}
	if countDrift(comparisons) != 0 {
		t.Fatalf("nothing should have drifted: %+v", comparisons)
	}

	err := benchGateVerdict(comparisons, unbudgeted, "budget.json")
	if err == nil {
		t.Fatal("the gate passed a run that measured no budgeted metric")
	}
	if code := exitCodeOf(t, err); code != exitBadNews {
		t.Fatalf("exit code = %d, want %d (the command ran; the answer is bad news)", code, exitBadNews)
	}
	if !strings.Contains(err.Error(), "frames.p95Ms") || !strings.Contains(err.Error(), "harness-window") {
		t.Fatalf("the error names neither the metric nor the fix: %v", err)
	}
}

// A metric the baseline never mentions is not a gate failure: a previous
// report carries every series the run that wrote it sampled, and a
// different engine legitimately samples fewer.
func TestAnUnbudgetedUnmeasuredMetricDoesNotFailTheGate(t *testing.T) {
	current := map[string]benchAggregate{
		"frames.p95Ms": {Runs: 1, P50: 10, Unit: "ms", LowerIsBetter: true},
	}
	baseline := benchBaseline{Aggregate: map[string]benchAggregate{
		"frames.p95Ms": {Runs: 1, P50: 10, Unit: "ms", LowerIsBetter: true},
		"domNodes.max": {Runs: 1, P50: 900, Unit: "count", LowerIsBetter: true},
	}}

	comparisons, unmeasured, unbudgeted := compareToBaselineDetailed(current, baseline)
	if len(unbudgeted) != 0 {
		t.Fatalf("unbudgeted = %v, want none: a derived reference is not an explicit budget", unbudgeted)
	}
	if !slices.Contains(unmeasured, "domNodes.max") {
		t.Errorf("unmeasured = %v, want the unsampled series reported", unmeasured)
	}
	if err := benchGateVerdict(comparisons, unbudgeted, "previous.json"); err != nil {
		t.Fatalf("gate failed on an unsampled series: %v", err)
	}
}

func TestDriftFailsTheGateWithTheSameCode(t *testing.T) {
	current := map[string]benchAggregate{"frames.p95Ms": {Runs: 1, P50: 40, Unit: "ms", LowerIsBetter: true}}
	baseline := benchBaseline{Metrics: map[string]benchTolerance{"frames.p95Ms": {Max: floatPtr(20)}}}

	comparisons, _, unbudgeted := compareToBaselineDetailed(current, baseline)
	err := benchGateVerdict(comparisons, unbudgeted, "budget.json")
	if err == nil {
		t.Fatal("drift passed the gate")
	}
	if code := exitCodeOf(t, err); code != exitBadNews {
		t.Fatalf("exit code = %d, want %d", code, exitBadNews)
	}
}

// The busy meter answers the question a vsync-quantised frame gap cannot:
// does one tick's main-thread work fit an N-ms budget. Its report arithmetic
// gets the same scrutiny as the frame half, and for the same reason — a
// `--baseline` gates CI on these numbers.

// withBusy stamps a busy summary onto a report. `benchTestReport` leaves the
// busy half at zero on purpose, which is the "this engine never armed the
// meter" case every unmeasured rule below turns on.
func withBusy(report perfReport, ticks int, p50, p95, max float64, fits ...perfBusyBudget) perfReport {
	report.Frontend.Busy = perfBusySummary{
		Ticks: ticks, P50Ms: p50, P95Ms: p95, MaxMs: max, MeanMs: p50, Budgets: fits,
	}
	return report
}

func TestBusyMetricsAreGateableInBothDirections(t *testing.T) {
	reports := []perfReport{
		withBusy(benchTestReport(1000, 60, 12, 8<<20), 900, 3.25, 9.5, 41,
			perfBusyBudget{BudgetMs: 6, WithinTicks: 700, WithinPct: 77.8},
			perfBusyBudget{BudgetMs: 16, WithinTicks: 880, WithinPct: 97.8}),
	}
	agg := aggregateBenchMetrics(reports)

	for name, want := range map[string]float64{
		"busy.p50Ms": 3.25, "busy.p95Ms": 9.5, "busy.maxMs": 41,
		"busy.fitPct.6ms": 77.8, "busy.fitPct.16ms": 97.8,
	} {
		metric, ok := agg[name]
		if !ok {
			t.Fatalf("%s missing from the aggregate", name)
		}
		if metric.P50 != want {
			t.Errorf("%s p50 = %v, want %v", name, metric.P50, want)
		}
	}
	// A percentile is lower-is-better and a FIT is higher-is-better. Getting
	// this backwards would report a renderer that got worse as an
	// improvement, which is the one direction a gate must never invert.
	if !agg["busy.p95Ms"].LowerIsBetter {
		t.Error("busy.p95Ms must be lower-is-better")
	}
	if agg["busy.fitPct.6ms"].LowerIsBetter {
		t.Error("busy.fitPct.6ms must be higher-is-better: it is the share of ticks that FIT")
	}
	if unit := agg["busy.fitPct.6ms"].Unit; unit != "pct" {
		t.Errorf("busy.fitPct.6ms unit = %q, want pct", unit)
	}

	// And it gates: a run that fits 6ms only 77.8% of the time fails a
	// budget demanding 90%.
	baseline := benchBaseline{Metrics: map[string]benchTolerance{
		"busy.fitPct.6ms": {Min: floatPtr(90)},
	}}
	comparisons, _, unbudgeted := compareToBaselineDetailed(agg, baseline)
	if len(comparisons) != 1 || !comparisons[0].Drift {
		t.Fatalf("comparisons = %+v, want one drifting fit metric", comparisons)
	}
	if err := benchGateVerdict(comparisons, unbudgeted, "budget.json"); err == nil {
		t.Fatal("a fit percentage below its floor passed the gate")
	}
}

func TestBusyMetricsAreAbsentWhenNoTickWasMeasured(t *testing.T) {
	// Zero busy time and no measurement are opposite findings; folding the
	// second in as the first reports a renderer that was never busy and a
	// 0% fit that reads as a catastrophic regression.
	unmeasured := benchTestReport(1000, 60, 12, 8<<20)
	agg := aggregateBenchMetrics([]perfReport{unmeasured})
	for _, name := range []string{"busy.p50Ms", "busy.p95Ms", "busy.maxMs"} {
		if _, ok := agg[name]; ok {
			t.Errorf("%s present for a run that measured no tick", name)
		}
	}

	// A budget on a metric this run could not measure is bad news, not a
	// pass — the same rule the frontend metrics already follow.
	baseline := benchBaseline{Metrics: map[string]benchTolerance{
		"busy.fitPct.6ms": {Min: floatPtr(90)},
	}}
	comparisons, unmeasuredNames, unbudgeted := compareToBaselineDetailed(agg, baseline)
	if len(comparisons) != 0 {
		t.Errorf("comparisons = %+v, want none", comparisons)
	}
	if !slices.Contains(unmeasuredNames, "busy.fitPct.6ms") {
		t.Errorf("unmeasured = %v, want the budgeted fit metric named", unmeasuredNames)
	}
	err := benchGateVerdict(comparisons, unbudgeted, "budget.json")
	if err == nil {
		t.Fatal("a budgeted-but-unmeasured busy metric passed the gate")
	}
	if code := exitCodeOf(t, err); code != exitBadNews {
		t.Fatalf("exit code = %d, want %d", code, exitBadNews)
	}
}

func TestBusyFitMetricsAreTheUnionOfTheRepeats(t *testing.T) {
	// The budget set is a run-time flag, not a constant, so the metric
	// vocabulary is derived from the reports. Repeats that disagree still
	// name every budget once, with only the repeats that carried it folded.
	first := withBusy(benchTestReport(1000, 60, 12, 8<<20), 900, 3, 9, 40,
		perfBusyBudget{BudgetMs: 6, WithinPct: 80})
	second := withBusy(benchTestReport(1000, 60, 12, 8<<20), 900, 3, 9, 40,
		perfBusyBudget{BudgetMs: 6, WithinPct: 60},
		perfBusyBudget{BudgetMs: 8, WithinPct: 95})
	agg := aggregateBenchMetrics([]perfReport{first, second})

	if got := agg["busy.fitPct.6ms"].Runs; got != 2 {
		t.Errorf("busy.fitPct.6ms folded %d runs, want 2", got)
	}
	eight, ok := agg["busy.fitPct.8ms"]
	if !ok {
		t.Fatal("busy.fitPct.8ms missing: a budget only one repeat carried must still be named")
	}
	if eight.Runs != 1 || eight.P50 != 95 {
		t.Errorf("busy.fitPct.8ms = %+v, want one run at 95", eight)
	}
}

func TestBusyFitMetricNameSpellsTheBudgetAsWritten(t *testing.T) {
	// A baseline file's key has to match what `--budgets` asked for, so the
	// number is spelled the way a caller writes it rather than padded.
	for budget, want := range map[float64]string{
		6: "busy.fitPct.6ms", 16: "busy.fitPct.16ms", 4.5: "busy.fitPct.4.5ms",
	} {
		if got := busyFitMetricName(budget); got != want {
			t.Errorf("busyFitMetricName(%v) = %q, want %q", budget, got, want)
		}
	}
}

func TestParseBudgetsMs(t *testing.T) {
	// Empty says nothing, which leaves the bridge's own default in force —
	// spelling the default in two places is how they drift apart.
	got, err := parseBudgetsMs("")
	if err != nil || got != nil {
		t.Errorf("parseBudgetsMs(\"\") = %v, %v; want nil, nil", got, err)
	}
	got, err = parseBudgetsMs(" 6, 8.5 ,16 ")
	if err != nil {
		t.Fatalf("parseBudgetsMs on a spaced list: %v", err)
	}
	if !slices.Equal(got, []float64{6, 8.5, 16}) {
		t.Errorf("parseBudgetsMs = %v, want [6 8.5 16]", got)
	}
	// A bad entry REFUSES rather than being skipped: silently gating on
	// fewer budgets than the caller typed is how a budget stops being
	// enforced with nobody the wiser.
	for _, bad := range []string{"6,,8", "6,fast", "6,0", "6,-2", "6,Inf"} {
		if _, err := parseBudgetsMs(bad); err == nil {
			t.Errorf("parseBudgetsMs(%q) accepted a bad entry", bad)
		}
	}
}

func TestRenderPerfReportBusyLine(t *testing.T) {
	report := withBusy(benchTestReport(1000, 60, 12, 8<<20), 900, 3.25, 9.5, 41,
		perfBusyBudget{BudgetMs: 6, WithinTicks: 700, WithinPct: 77.83},
		perfBusyBudget{BudgetMs: 16, WithinTicks: 880, WithinPct: 97.78})
	report.Frontend.Busy.Dropped = 4
	rendered := renderPerfReport(report)
	for _, want := range []string{
		"p50 3.25ms", "max 41.00ms", "over 900 ticks", "(4 dropped)",
		"fit 6ms 77.8% / 16ms 97.8%",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered report is missing %q:\n%s", want, rendered)
		}
	}

	// A run that measured no tick prints no busy line at all: a row of
	// zeros reads as "the main thread was never busy".
	quiet := renderPerfReport(benchTestReport(1000, 60, 12, 8<<20))
	if strings.Contains(quiet, "busy") {
		t.Errorf("an unmeasured run printed a busy line:\n%s", quiet)
	}
}
