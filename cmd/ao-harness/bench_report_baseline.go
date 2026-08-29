package main

import (
	"fmt"
	"sort"
)

// benchBaseline is what --baseline reads. Two accepted shapes, because the
// two things a caller has are a hand-written budget and YESTERDAY'S
// REPORT: `metrics` is the budget, `aggregate` is a previous report's own
// fold, whose p50 becomes the reference value under the default tolerance.
type benchBaseline struct {
	benchReportIdentity
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
