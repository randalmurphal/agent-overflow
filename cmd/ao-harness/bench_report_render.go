package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

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

func frontendMeterLabel(f perfFrontendSummary, meter string) string {
	if frontendMeterMeasured(f, meter) {
		return "measured"
	}
	for _, unavailable := range f.UnavailableMeters {
		if unavailable == meter {
			return "unavailable"
		}
	}
	return "unselected"
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
		if frontendMeterMeasured(*f, "frames") {
			fmt.Fprintf(&b, "  frames  %.1f fps over %d frames; p50 %.1fms p95 %.1fms p99 %.1fms max %.1fms; %d long (> %.0fms)\n",
				f.Frames.FPS, f.Frames.Frames, f.Frames.P50Ms, f.Frames.P95Ms, f.Frames.P99Ms,
				f.Frames.MaxMs, f.Frames.LongFrames, f.Frames.LongFrameMs)
		} else {
			fmt.Fprintf(&b, "  frames  %s\n", frontendMeterLabel(*f, "frames"))
		}
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
		if frontendMeterMeasured(*f, "longtask") || frontendMeterMeasured(*f, "layout-shift") || frontendMeterMeasured(*f, "event") {
			fmt.Fprintf(&b, "  tasks   %s\n", renderFrontendTaskSummary(*f))
		}
		if frontendMeterMeasured(*f, "dom") || frontendMeterMeasured(*f, "memory") {
			fmt.Fprintf(&b, "  page    %s\n", renderFrontendPageSummary(*f))
		}
		if len(f.UnavailableMeters) > 0 {
			fmt.Fprintf(&b, "  meters unavailable in this engine: %s\n", strings.Join(f.UnavailableMeters, ", "))
		}
	}
	fmt.Fprintf(&b, "  backend go heap max %s; goroutines max %s; rss max %s; webview rss max %s\n",
		backendSeriesCell(report.Backend.HeapBytes, "bytes"), backendSeriesCell(report.Backend.Goroutines, "count"),
		backendSeriesCell(report.Backend.RSSBytes, "bytes"), backendSeriesCell(report.Backend.WebviewRSSBytes, "bytes"))
	return b.String()
}

func backendSeriesCell(series perfSeries, unit string) string {
	if series.Count == 0 {
		return "-"
	}
	if unit == "bytes" {
		return humanBytes(uint64(series.Max))
	}
	return formatBenchValue(series.Max, unit)
}

func renderFrontendTaskSummary(f perfFrontendSummary) string {
	parts := make([]string, 0, 3)
	if frontendMeterMeasured(f, "longtask") {
		parts = append(parts, fmt.Sprintf("%d long tasks (worst %.1fms)", f.LongTasks, f.LongestTaskMs))
	}
	if frontendMeterMeasured(f, "layout-shift") {
		parts = append(parts, fmt.Sprintf("layout shift %.4f", f.LayoutShift))
	}
	if frontendMeterMeasured(f, "event") {
		parts = append(parts, fmt.Sprintf("%d slow events", f.SlowEvents))
	}
	return strings.Join(parts, "; ")
}

func renderFrontendPageSummary(f perfFrontendSummary) string {
	parts := make([]string, 0, 2)
	if frontendMeterMeasured(f, "dom") && f.DomNodes.Count > 0 {
		parts = append(parts, fmt.Sprintf("dom %.0f -> %.0f (max %.0f)", f.DomNodes.Min, f.DomNodes.Last, f.DomNodes.Max))
	} else if frontendMeterMeasured(f, "dom") {
		parts = append(parts, "dom not sampled")
	}
	if frontendMeterMeasured(f, "memory") && f.HeapBytes.Count > 0 {
		parts = append(parts, fmt.Sprintf("js heap max %s", humanBytes(uint64(f.HeapBytes.Max))))
	} else if frontendMeterMeasured(f, "memory") {
		parts = append(parts, "js heap not sampled")
	}
	return strings.Join(parts, "; ")
}
