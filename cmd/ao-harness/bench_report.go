package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

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
	TimeOriginMs      float64          `json:"timeOriginMs,omitempty"`
	Meters            []string         `json:"meters"`
	UnavailableMeters []string         `json:"unavailableMeters"`
	Frames            perfFrameSummary `json:"frames"`
	Busy              perfBusySummary  `json:"busy"`
	LongTasks         int              `json:"longTasks"`
	LongestTaskMs     float64          `json:"longestTaskMs"`
	LayoutShift       float64          `json:"layoutShift"`
	SlowEvents        int              `json:"slowEvents"`
	DomNodes          perfSeries       `json:"domNodes"`
	HeapBytes         perfSeries       `json:"heapBytes"`
	Samples           int              `json:"samples"`
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
}

func decodePerfReport(raw json.RawMessage) (perfReport, error) {
	var out perfReport
	if err := json.Unmarshal(raw, &out); err != nil {
		return perfReport{}, fmt.Errorf("decode perf report: %w", err)
	}
	return out, nil
}

// parseBudgetsMs reads the `--budgets` flag: a comma-separated list of
// main-thread budgets in milliseconds. Empty means "say nothing", which
// leaves the bridge's own default (6, 8, 16) in force — spelling the
// default here too would make two places to change it.
//
// A budget that does not parse, or is not positive, is a REFUSAL rather
// than a skipped entry: the flag exists to say which numbers to gate on,
// and quietly gating on fewer than the caller typed is how a budget stops
// being enforced without anyone noticing.
func parseBudgetsMs(flag string) ([]float64, error) {
	trimmed := strings.TrimSpace(flag)
	if trimmed == "" {
		return nil, nil
	}
	fields := strings.Split(trimmed, ",")
	out := make([]float64, 0, len(fields))
	for _, field := range fields {
		text := strings.TrimSpace(field)
		if text == "" {
			return nil, usagef("--budgets has an empty entry in %q", flag)
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, usagef("--budgets entry %q is not a number", text)
		}
		if value <= 0 || math.IsInf(value, 0) {
			return nil, usagef("--budgets entry %q must be a positive number of milliseconds", text)
		}
		out = append(out, value)
	}
	return out, nil
}

// benchMetric names one number a bench compares over time. The direction
// is declared here rather than inferred from the name, because that is the
// only thing that decides whether "bigger than the baseline" is drift or
// an improvement.
type benchMetric struct {
	Name string
	// Unit labels the terminal column: "ms", "fps", "bytes", "count".
	Unit string
	// LowerIsBetter flips the comparison. Exactly one metric here is
	// higher-is-better (fps), which is precisely why the flag exists.
	LowerIsBetter bool
	read          func(perfReport) (float64, bool)
}

func frontendMetric(read func(perfFrontendSummary) float64) func(perfReport) (float64, bool) {
	return func(r perfReport) (float64, bool) {
		if r.Frontend == nil {
			return 0, false
		}
		return read(*r.Frontend), true
	}
}

// frontendSeriesMetric is frontendMetric for a series-shaped value: a
// series the page never sampled (WebKitGTK has no performance.memory)
// is absent from the aggregate rather than a column of zeros.
func frontendSeriesMetric(read func(perfFrontendSummary) perfSeries) func(perfReport) (float64, bool) {
	return func(r perfReport) (float64, bool) {
		if r.Frontend == nil {
			return 0, false
		}
		series := read(*r.Frontend)
		if series.Count == 0 {
			return 0, false
		}
		return series.Max, true
	}
}

// frontendBusyMetric is frontendMetric for a busy figure. A run whose page
// never MEASURED a tick — the meter was not armed, or this engine has no
// MessageChannel to probe with — is ABSENT rather than zero, because zero
// busy time reads as a flawless renderer.
func frontendBusyMetric(read func(perfBusySummary) float64) func(perfReport) (float64, bool) {
	return func(r perfReport) (float64, bool) {
		if r.Frontend == nil || r.Frontend.Busy.Ticks == 0 {
			return 0, false
		}
		return read(r.Frontend.Busy), true
	}
}

// busyFitMetricName is how a budget becomes a gateable metric name. The
// number is spelled the way the caller wrote it (`6`, `4.5`), so a
// hand-written baseline key matches what `--budgets` asked for.
func busyFitMetricName(budgetMs float64) string {
	return "busy.fitPct." + strconv.FormatFloat(budgetMs, 'f', -1, 64) + "ms"
}

// benchBudgetMetrics derives one metric per budget the REPORTS carry.
// Unlike everything in benchMetrics the budget set is a run-time flag, so
// the vocabulary cannot be a constant here; taking the union across repeats
// means a bench whose repeats disagreed (a page rearmed mid-run) still
// names every budget exactly once, with the repeats that lack it simply
// not folded in — the same rule every other unmeasured metric follows.
func benchBudgetMetrics(reports []perfReport) []benchMetric {
	seen := map[float64]bool{}
	for _, report := range reports {
		if report.Frontend == nil {
			continue
		}
		for _, budget := range report.Frontend.Busy.Budgets {
			seen[budget.BudgetMs] = true
		}
	}
	budgets := make([]float64, 0, len(seen))
	for budget := range seen {
		budgets = append(budgets, budget)
	}
	sort.Float64s(budgets)
	out := make([]benchMetric, 0, len(budgets))
	for _, budgetMs := range budgets {
		out = append(out, benchMetric{
			// Higher is better: this is the share of ticks that FIT.
			Name: busyFitMetricName(budgetMs), Unit: "pct", LowerIsBetter: false,
			read: func(r perfReport) (float64, bool) {
				if r.Frontend == nil || r.Frontend.Busy.Ticks == 0 {
					return 0, false
				}
				for _, fit := range r.Frontend.Busy.Budgets {
					if fit.BudgetMs == budgetMs {
						return fit.WithinPct, true
					}
				}
				return 0, false
			},
		})
	}
	return out
}

func backendMetric(read func(perfBackendReport) perfSeries) func(perfReport) (float64, bool) {
	return func(r perfReport) (float64, bool) {
		series := read(r.Backend)
		if series.Count == 0 {
			return 0, false
		}
		return series.Max, true
	}
}

// benchMetrics is the report's whole vocabulary, in print order. A metric
// that a run could not measure (no frontend answered, no /proc to walk) is
// ABSENT from that run rather than zero: a zero would fold into the
// aggregate and quietly halve a mean.
func benchMetrics() []benchMetric {
	return []benchMetric{
		{Name: "duration.ms", Unit: "ms", LowerIsBetter: true,
			read: func(r perfReport) (float64, bool) { return float64(r.DurationMs), true }},
		{Name: "frames.fps", Unit: "fps", LowerIsBetter: false,
			read: frontendMetric(func(f perfFrontendSummary) float64 { return f.Frames.FPS })},
		{Name: "frames.p50Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendMetric(func(f perfFrontendSummary) float64 { return f.Frames.P50Ms })},
		{Name: "frames.p95Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendMetric(func(f perfFrontendSummary) float64 { return f.Frames.P95Ms })},
		{Name: "frames.p99Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendMetric(func(f perfFrontendSummary) float64 { return f.Frames.P99Ms })},
		{Name: "frames.maxMs", Unit: "ms", LowerIsBetter: true,
			read: frontendMetric(func(f perfFrontendSummary) float64 { return f.Frames.MaxMs })},
		{Name: "frames.long", Unit: "count", LowerIsBetter: true,
			read: frontendMetric(func(f perfFrontendSummary) float64 { return float64(f.Frames.LongFrames) })},
		{Name: "busy.p50Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendBusyMetric(func(b perfBusySummary) float64 { return b.P50Ms })},
		{Name: "busy.p95Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendBusyMetric(func(b perfBusySummary) float64 { return b.P95Ms })},
		{Name: "busy.maxMs", Unit: "ms", LowerIsBetter: true,
			read: frontendBusyMetric(func(b perfBusySummary) float64 { return b.MaxMs })},
		{Name: "longTasks", Unit: "count", LowerIsBetter: true,
			read: frontendMetric(func(f perfFrontendSummary) float64 { return float64(f.LongTasks) })},
		{Name: "longestTaskMs", Unit: "ms", LowerIsBetter: true,
			read: frontendMetric(func(f perfFrontendSummary) float64 { return f.LongestTaskMs })},
		{Name: "layoutShift", Unit: "score", LowerIsBetter: true,
			read: frontendMetric(func(f perfFrontendSummary) float64 { return f.LayoutShift })},
		{Name: "domNodes.max", Unit: "count", LowerIsBetter: true,
			read: frontendSeriesMetric(func(f perfFrontendSummary) perfSeries { return f.DomNodes })},
		{Name: "jsHeap.maxBytes", Unit: "bytes", LowerIsBetter: true,
			read: frontendSeriesMetric(func(f perfFrontendSummary) perfSeries { return f.HeapBytes })},
		{Name: "goHeap.maxBytes", Unit: "bytes", LowerIsBetter: true,
			read: backendMetric(func(b perfBackendReport) perfSeries { return b.HeapBytes })},
		{Name: "goroutines.max", Unit: "count", LowerIsBetter: true,
			read: backendMetric(func(b perfBackendReport) perfSeries { return b.Goroutines })},
		{Name: "webviewRss.maxBytes", Unit: "bytes", LowerIsBetter: true,
			read: backendMetric(func(b perfBackendReport) perfSeries { return b.WebviewRSSBytes })},
	}
}

// benchAggregate folds one metric across the repeats of a bench.
type benchAggregate struct {
	Runs int     `json:"runs"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	// Unit and LowerIsBetter travel with the number so a report read a
	// month later still says which direction is good.
	Unit          string `json:"unit"`
	LowerIsBetter bool   `json:"lowerIsBetter"`
}

// aggregateBenchMetrics folds every metric over the per-repeat reports.
// Percentiles over a handful of samples are nearest-rank, not
// interpolated: with three repeats an interpolated p95 invents a number
// between two runs that never happened.
func aggregateBenchMetrics(reports []perfReport) map[string]benchAggregate {
	metrics := append(benchMetrics(), benchBudgetMetrics(reports)...)
	out := make(map[string]benchAggregate, len(metrics))
	for _, metric := range metrics {
		values := make([]float64, 0, len(reports))
		for _, report := range reports {
			if value, ok := metric.read(report); ok {
				values = append(values, value)
			}
		}
		if len(values) == 0 {
			continue
		}
		sort.Float64s(values)
		out[metric.Name] = benchAggregate{
			Runs:          len(values),
			P50:           nearestRank(values, 0.50),
			P95:           nearestRank(values, 0.95),
			Min:           values[0],
			Max:           values[len(values)-1],
			Unit:          metric.Unit,
			LowerIsBetter: metric.LowerIsBetter,
		}
	}
	return out
}

// nearestRank picks the sorted value at ceil(fraction * n), 1-indexed.
// With one sample every percentile is that sample, which is the honest
// answer for a single run.
func nearestRank(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(fraction * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// benchTolerance is one metric's drift budget. `max`/`maxPctOver` bound a
// lower-is-better metric; `min`/`minPctUnder` bound a higher-is-better
// one. `value` is the reference the percentage applies to; a file that
// carries only `max` needs none.
type benchTolerance struct {
	Value       *float64 `json:"value,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	MaxPctOver  *float64 `json:"maxPctOver,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	MinPctUnder *float64 `json:"minPctUnder,omitempty"`
}

// benchBaseline is what --baseline reads. Two accepted shapes, because the
// two things a caller has are a hand-written budget and YESTERDAY'S
// REPORT: `metrics` is the budget, `aggregate` is a previous report's own
// fold, whose p50 becomes the reference value under the default tolerance.
type benchBaseline struct {
	Workload  string                    `json:"workload,omitempty"`
	Metrics   map[string]benchTolerance `json:"metrics,omitempty"`
	Aggregate map[string]benchAggregate `json:"aggregate,omitempty"`
}

// benchDefaultTolerancePct is how far a metric may drift from a reference
// value that carried no explicit budget. Machine variance on a laptop
// running a browser is real; a quarter is loose enough not to cry wolf and
// tight enough to catch a regression worth a look.
const benchDefaultTolerancePct = 25.0

// benchComparison is one metric's verdict.
type benchComparison struct {
	Metric    string  `json:"metric"`
	Current   float64 `json:"current"`
	Reference float64 `json:"reference,omitempty"`
	Limit     float64 `json:"limit"`
	Drift     bool    `json:"drift"`
	// Note says which rule produced Limit, so a surprising verdict is
	// self-explaining rather than needing the baseline file reopened.
	Note string `json:"note"`
}

// compareToBaseline evaluates every metric the baseline has an opinion
// about. A metric the baseline does not mention is not compared, and a
// baseline that mentions a metric this run could not measure is reported
// as unmeasured rather than as a pass.
//
// Unmeasured is split in two, because the two halves are opposite
// verdicts. A metric that came from a previous report's `aggregate` and
// is missing today is often legitimate — a series meter this engine
// never samples (jsHeap on WebKitGTK) is absent by design. A metric an
// EXPLICIT `metrics` budget names is a caller writing down "gate this",
// and silence is the one answer a gate must never give: a headless run
// whose page answered nothing produced zero comparisons and exited 0,
// which is a green light for a bench that measured nothing at all.
func compareToBaseline(current map[string]benchAggregate, baseline benchBaseline) ([]benchComparison, []string) {
	comparisons, unmeasured, _ := compareToBaselineDetailed(current, baseline)
	return comparisons, unmeasured
}

func compareToBaselineDetailed(current map[string]benchAggregate, baseline benchBaseline) (out []benchComparison, unmeasured, unmeasuredBudgeted []string) {
	tolerances := map[string]benchTolerance{}
	for name, agg := range baseline.Aggregate {
		value := agg.P50
		tolerances[name] = benchTolerance{Value: &value}
	}
	for name, tol := range baseline.Metrics {
		// An explicit budget wins over a reference derived from a report:
		// the caller wrote it down on purpose.
		tolerances[name] = tol
	}

	names := make([]string, 0, len(tolerances))
	for name := range tolerances {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		agg, ok := current[name]
		if !ok {
			unmeasured = append(unmeasured, name)
			if _, explicit := baseline.Metrics[name]; explicit {
				unmeasuredBudgeted = append(unmeasuredBudgeted, name)
			}
			continue
		}
		out = append(out, evaluateTolerance(name, agg, tolerances[name]))
	}
	return out, unmeasured, unmeasuredBudgeted
}

func evaluateTolerance(name string, agg benchAggregate, tol benchTolerance) benchComparison {
	result := benchComparison{Metric: name, Current: agg.P50}
	if tol.Value != nil {
		result.Reference = *tol.Value
	}
	if agg.LowerIsBetter {
		limit, note, resolved := upperLimit(tol)
		result.Limit, result.Note = limit, note
		result.Drift = resolved && agg.P50 > limit
		return result
	}
	limit, note, resolved := lowerLimit(tol)
	result.Limit, result.Note = limit, note
	result.Drift = resolved && agg.P50 < limit
	return result
}

// upperLimit resolves a lower-is-better budget. An explicit `max` is a
// hard ceiling and wins; otherwise a percentage over the reference
// applies, defaulting to benchDefaultTolerancePct.
//
// The third return says whether a limit was resolved AT ALL, which is not
// the same question as "is it above zero": `{"max": 0}` is a caller
// writing down "this metric must stay at zero" — the strictest budget the
// file can express — and gating drift on a positive limit would ignore it
// silently, which is worse than any wrong verdict.
func upperLimit(tol benchTolerance) (float64, string, bool) {
	if tol.Max != nil {
		return *tol.Max, "max", true
	}
	if tol.Value == nil {
		return 0, "no reference", false
	}
	pct := benchDefaultTolerancePct
	note := fmt.Sprintf("+%.0f%% (default)", pct)
	if tol.MaxPctOver != nil {
		pct = *tol.MaxPctOver
		note = fmt.Sprintf("+%.0f%%", pct)
	}
	return *tol.Value * (1 + pct/100), note, true
}

func lowerLimit(tol benchTolerance) (float64, string, bool) {
	if tol.Min != nil {
		return *tol.Min, "min", true
	}
	if tol.Value == nil {
		return 0, "no reference", false
	}
	pct := benchDefaultTolerancePct
	note := fmt.Sprintf("-%.0f%% (default)", pct)
	if tol.MinPctUnder != nil {
		pct = *tol.MinPctUnder
		note = fmt.Sprintf("-%.0f%%", pct)
	}
	return *tol.Value * (1 - pct/100), note, true
}

func formatBenchValue(value float64, unit string) string {
	switch unit {
	case "bytes":
		return humanBytes(uint64(math.Max(value, 0)))
	case "count":
		return fmt.Sprintf("%.0f", value)
	case "score":
		return fmt.Sprintf("%.4f", value)
	case "pct":
		return fmt.Sprintf("%.1f%%", value)
	default:
		return fmt.Sprintf("%.1f", value)
	}
}

// renderBusyFit is the budget-fit strip: `fit 6ms 72.1% / 8ms 88.4%`.
// Empty for a run that carried no budget at all, so the caller can skip
// the line rather than print a label with nothing after it.
func renderBusyFit(budgets []perfBusyBudget) string {
	if len(budgets) == 0 {
		return ""
	}
	parts := make([]string, 0, len(budgets))
	for _, budget := range budgets {
		parts = append(parts, fmt.Sprintf("%sms %.1f%%",
			strconv.FormatFloat(budget.BudgetMs, 'f', -1, 64), budget.WithinPct))
	}
	return "fit " + strings.Join(parts, " / ")
}

// renderBusyWorst is the worst-tick strip: the run's most expensive main
// thread ticks, worst first, each with the moment it started.
//
// TWO CLOCKS, on purpose. `atMs` is the page clock (`performance.now()`),
// which is what a Chromium trace and the rest of this report are on; the
// wall-clock column is that same instant through `timeOrigin`, which is
// what a backend log or a ui-trace record is on. A reader correlating a
// stall has one of the two and would otherwise have to do the arithmetic
// by hand. The wall column is omitted entirely when the page reported no
// time origin, rather than printing the epoch.
func renderBusyWorst(worst []perfBusyWorstTick, timeOriginMs float64) string {
	if len(worst) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  worst   %d worst tick(s), page clock; correlate with a trace\n", len(worst))
	rows := make([][]string, 0, len(worst))
	for i, tick := range worst {
		wall := "-"
		if timeOriginMs > 0 {
			wall = time.UnixMilli(int64(timeOriginMs + tick.AtMs)).Format("15:04:05.000")
		}
		rows = append(rows, []string{
			fmt.Sprint(i + 1),
			fmt.Sprintf("%.2f", tick.BusyMs),
			fmt.Sprintf("%.1f", tick.AtMs),
			wall,
		})
	}
	b.WriteString(tableString([]string{"    #", "BUSYMS", "AT(PAGE)", "AT(WALL)"}, rows))
	return b.String()
}

// droppedSuffix names the ticks whose measurement could not be attributed
// to one frame. Silent at zero, which is the normal case.
func droppedSuffix(dropped int) string {
	if dropped == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d dropped)", dropped)
}

// renderPerfReport is the terminal form of one perf run, shared by
// `perf stop` and the per-repeat lines a bench prints.
func renderPerfReport(report perfReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "perf run %s: %d samples over %dms (interval %dms)\n",
		report.RunID, report.Samples, report.DurationMs, report.SampleMs)
	if report.Frontend == nil {
		fmt.Fprintf(&b, "  frontend: %s\n", orDash(report.FrontendError))
	} else {
		f := report.Frontend
		fmt.Fprintf(&b, "  frames  %.1f fps over %d frames; p50 %.1fms p95 %.1fms p99 %.1fms max %.1fms; %d long (> %.0fms)\n",
			f.Frames.FPS, f.Frames.Frames, f.Frames.P50Ms, f.Frames.P95Ms, f.Frames.P99Ms,
			f.Frames.MaxMs, f.Frames.LongFrames, f.Frames.LongFrameMs)
		// The busy line is printed only when a tick was measured. A row of
		// zeros would read as "the main thread was never busy", which is
		// the opposite of "this engine never armed the meter".
		if f.Busy.Ticks > 0 {
			fmt.Fprintf(&b, "  busy    p50 %.2fms p95 %.2fms max %.2fms mean %.2fms over %d ticks%s\n",
				f.Busy.P50Ms, f.Busy.P95Ms, f.Busy.MaxMs, f.Busy.MeanMs, f.Busy.Ticks,
				droppedSuffix(f.Busy.Dropped))
			if fit := renderBusyFit(f.Busy.Budgets); fit != "" {
				fmt.Fprintf(&b, "  budget  %s\n", fit)
			}
			b.WriteString(renderBusyWorst(f.Busy.Worst, f.TimeOriginMs))
		}
		fmt.Fprintf(&b, "  tasks   %d long tasks (worst %.1fms); layout shift %.4f; %d slow events\n",
			f.LongTasks, f.LongestTaskMs, f.LayoutShift, f.SlowEvents)
		fmt.Fprintf(&b, "  page    dom %.0f -> %.0f (max %.0f); js heap max %s\n",
			f.DomNodes.Min, f.DomNodes.Last, f.DomNodes.Max, humanBytes(uint64(f.HeapBytes.Max)))
		if len(f.UnavailableMeters) > 0 {
			fmt.Fprintf(&b, "  meters unavailable in this engine: %s\n", strings.Join(f.UnavailableMeters, ", "))
		}
	}
	fmt.Fprintf(&b, "  backend go heap max %s; goroutines max %.0f; rss max %s; webview rss max %s\n",
		humanBytes(uint64(report.Backend.HeapBytes.Max)), report.Backend.Goroutines.Max,
		humanBytes(uint64(report.Backend.RSSBytes.Max)), humanBytes(uint64(report.Backend.WebviewRSSBytes.Max)))
	return b.String()
}
