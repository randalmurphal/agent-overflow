package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

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
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
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

// frontendMeterMetric admits a scalar only when the run actually armed the
// meter that owns it. A frontend summary keeps the complete wire shape for
// stable decoding, so an unselected value is otherwise indistinguishable
// from a measured zero. Nil Meters is retained for decoding old in-memory
// fixtures; reports written by the bridge always carry the list.
func frontendMeterMetric(meter string, read func(perfFrontendSummary) float64) func(perfReport) (float64, bool) {
	return func(r perfReport) (float64, bool) {
		if r.Frontend == nil || !frontendMeterMeasured(*r.Frontend, meter) {
			return 0, false
		}
		return read(*r.Frontend), true
	}
}

func frontendMeterMeasured(f perfFrontendSummary, meter string) bool {
	if f.Meters != nil {
		for _, selected := range f.Meters {
			if selected == meter {
				for _, unavailable := range f.UnavailableMeters {
					if unavailable == meter {
						return false
					}
				}
				return true
			}
		}
		return false
	}
	// A nil meter list is the pre-meter-aware report shape. Keep old decoded
	// fixtures readable, while a present empty list means no meters.
	for _, unavailable := range f.UnavailableMeters {
		if unavailable == meter {
			return false
		}
	}
	return true
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

func frontendMeterSeriesMetric(meter string, read func(perfFrontendSummary) perfSeries) func(perfReport) (float64, bool) {
	return func(r perfReport) (float64, bool) {
		if r.Frontend == nil || !frontendMeterMeasured(*r.Frontend, meter) {
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
		if r.Frontend == nil || !frontendMeterMeasured(*r.Frontend, "busy") || r.Frontend.Busy.Ticks == 0 {
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
				if r.Frontend == nil || !frontendMeterMeasured(*r.Frontend, "busy") || r.Frontend.Busy.Ticks == 0 {
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
			read: frontendMeterMetric("frames", func(f perfFrontendSummary) float64 { return f.Frames.FPS })},
		{Name: "frames.p50Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendMeterMetric("frames", func(f perfFrontendSummary) float64 { return f.Frames.P50Ms })},
		{Name: "frames.p95Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendMeterMetric("frames", func(f perfFrontendSummary) float64 { return f.Frames.P95Ms })},
		{Name: "frames.p99Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendMeterMetric("frames", func(f perfFrontendSummary) float64 { return f.Frames.P99Ms })},
		{Name: "frames.maxMs", Unit: "ms", LowerIsBetter: true,
			read: frontendMeterMetric("frames", func(f perfFrontendSummary) float64 { return f.Frames.MaxMs })},
		{Name: "frames.long", Unit: "count", LowerIsBetter: true,
			read: frontendMeterMetric("frames", func(f perfFrontendSummary) float64 { return float64(f.Frames.LongFrames) })},
		{Name: "busy.p50Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendBusyMetric(func(b perfBusySummary) float64 { return b.P50Ms })},
		{Name: "busy.p95Ms", Unit: "ms", LowerIsBetter: true,
			read: frontendBusyMetric(func(b perfBusySummary) float64 { return b.P95Ms })},
		{Name: "busy.maxMs", Unit: "ms", LowerIsBetter: true,
			read: frontendBusyMetric(func(b perfBusySummary) float64 { return b.MaxMs })},
		{Name: "longTasks", Unit: "count", LowerIsBetter: true,
			read: frontendMeterMetric("longtask", func(f perfFrontendSummary) float64 { return float64(f.LongTasks) })},
		{Name: "longestTaskMs", Unit: "ms", LowerIsBetter: true,
			read: frontendMeterMetric("longtask", func(f perfFrontendSummary) float64 { return f.LongestTaskMs })},
		{Name: "longAnimationFrames", Unit: "count", LowerIsBetter: true,
			read: frontendMeterMetric("loaf", func(f perfFrontendSummary) float64 { return float64(f.LongAnimationFrames) })},
		{Name: "longestAnimationFrameMs", Unit: "ms", LowerIsBetter: true,
			read: frontendMeterMetric("loaf", func(f perfFrontendSummary) float64 { return f.LongestAnimationFrameMs })},
		{Name: "layoutShift", Unit: "score", LowerIsBetter: true,
			read: frontendMeterMetric("layout-shift", func(f perfFrontendSummary) float64 { return f.LayoutShift })},
		{Name: "slowEvents", Unit: "count", LowerIsBetter: true,
			read: frontendMeterMetric("event", func(f perfFrontendSummary) float64 { return float64(f.SlowEvents) })},
		{Name: "domNodes.max", Unit: "count", LowerIsBetter: true,
			read: frontendMeterSeriesMetric("dom", func(f perfFrontendSummary) perfSeries { return f.DomNodes })},
		{Name: "jsHeap.maxBytes", Unit: "bytes", LowerIsBetter: true,
			read: frontendMeterSeriesMetric("memory", func(f perfFrontendSummary) perfSeries { return f.HeapBytes })},
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
