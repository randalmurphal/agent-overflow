package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A bench run is a scripted workload driven against a REAL attached page,
// with the perf meters armed around it. Three things make it a bench
// rather than a soak: it seeds its own fixture, has an explicit end condition,
// and writes a report a later run can be compared against.
//
// WHICH END CONDITION. Ordinary workloads wait for a completion signal. Two
// candidates exist, and they answer different
// questions. `harness:mock`'s `scenario_done` says the MOCK finished
// writing its script, which is upstream of everything a bench measures:
// the app has not yet parsed the tail, triaged it, persisted it, or
// rendered it. `provider:turn_completed` is emitted by triage after the
// terminal `result` envelope has been classified and the round closed, so
// it is the first moment the whole pipeline under test is done. That is
// the one those workloads wait on, per thread id. A runner-timed workload
// instead proves every pane is still rendering throughout its requested
// duration, then interrupts the infinite mock turn through the production
// API. A short settle follows either path, so the
// frames the tail produced land in a sample before the meters stop.
//
// WHY IT DOES NOT BOOT AN INSTANCE. Perf needs a page: the frame meters
// live in the document. A backend this command started would be headless
// and could not answer a single ui-query, so "boot one for you" would just
// move the failure later. It attaches, and a bridge that does not answer
// is an error naming the two ways to get a page onto the instance.

const (
	// These mirror the backend's perf sampling defaults. The CLI uses them
	// only to stamp the measurement identity before the first RPC.
	harnessPerfDefaultSampleMs = 1000
	harnessPerfMinSampleMs     = 250

	// benchSettleMs is how long the meters keep running after the turn
	// completes. One default sample interval plus a beat: the last frames
	// of a turn are the ones a regression usually lives in.
	benchSettleMs = 1200
	// benchTurnTimeout bounds one workload's turn. bench-giant-turn writes
	// 750 wire lines; a minute is far more than any of them need and short
	// enough that a wedged run fails inside a coffee break.
	benchTurnTimeout = 90 * time.Second
	// benchBridgeTimeout bounds the "is a page attached" probe and every
	// poll that waits for the page to catch up.
	benchBridgeTimeout = 30 * time.Second
	// The default is the first bounded stage, not the final endurance claim.
	// Longer runs are explicit through --duration and pair with the external
	// WebView process ceiling. An accidental bare command must not spend ten
	// minutes growing an unbounded synthetic response.
	benchActiveDefaultDuration = 30 * time.Second
	benchActiveMinimumDuration = 30 * time.Second
	benchDirName               = "bench"
)

// benchWorkload is one named workload: the fixture it needs and the thing
// it does between arming and stopping the meters.
type benchWorkload struct {
	Name string
	// Scenario is the library entry the mock runs, empty for a workload
	// that drives no provider turn.
	Scenario string
	Summary  string
	// DefaultDuration marks a runner-timed workload. Zero means its own
	// completion event ends it. A positive value means the driver keeps the
	// turn active for this long, then interrupts it cleanly.
	DefaultDuration time.Duration
	// MinimumDuration rejects a runner-timed sample too short to exercise the
	// behavior it claims. It must be positive whenever DefaultDuration is.
	MinimumDuration time.Duration
	seed            func(run *benchRun) (json.RawMessage, error)
	// prepare runs after the page has been reloaded and pointed at the
	// first thread, and BEFORE the meters (and the trace) are armed. It is
	// where a workload puts the app into the shape it wants to measure —
	// mounting the other panes, for instance — so that setup cost stays out
	// of the window. Nil for a workload whose fixture is enough.
	prepare     func(ctx context.Context, run *benchRun) error
	beforeStart func(ctx context.Context, run *benchRun) error
	drive       func(ctx context.Context, run *benchRun) error
}

func (w benchWorkload) description(duration time.Duration) string {
	if w.DefaultDuration == 0 {
		return w.Summary
	}
	return fmt.Sprintf("%s for %s", w.Summary, duration)
}

func benchWorkloads() []benchWorkload {
	return []benchWorkload{
		{
			Name:     "burst-stream",
			Scenario: "bench-burst-stream",
			Summary:  "sustained text-delta flood with chunked partial writes",
			seed:     seedSingleThread,
			drive:    driveOneTurn,
		},
		{
			Name:     "mixed-turn",
			Scenario: "bench-mixed-turn",
			Summary:  "thinking, Read/Bash output, inline diff and rich text at varied cadences, completing naturally",
			seed:     seedSingleThread,
			drive:    driveOneTurn,
		},
		{
			Name:     "giant-turn",
			Scenario: "bench-giant-turn",
			Summary:  "one turn producing 225 items (tool pairs plus text blocks)",
			seed:     seedSingleThread,
			drive:    driveOneTurn,
		},
		{
			Name:     "subagent-fanout",
			Scenario: "bench-subagent-fanout",
			Summary:  "three bounded async subagents streaming into their own cards",
			seed:     seedSingleThread,
			drive:    driveOneTurn,
		},
		{
			Name:     "multi-pane-stream",
			Scenario: "bench-burst-stream",
			Summary:  fmt.Sprintf("%d panes side by side, each streaming a delta flood at once", benchMultiPaneCount),
			seed:     seedMultiPaneThreads,
			prepare:  openPanesForMultiPaneStream,
			drive:    driveMultiPaneStream,
		},
		{
			Name:     "active-multi-pane",
			Scenario: "bench-active-stream",
			Summary: fmt.Sprintf("%d open panes, %d streaming one long paced rich-Markdown turn",
				benchActivePaneCount, benchActiveStreamCount),
			DefaultDuration: benchActiveDefaultDuration,
			MinimumDuration: benchActiveMinimumDuration,
			seed:            seedActiveMultiPaneThreads,
			prepare:         prepareActiveMultiPaneStream,
			drive:           driveActiveMultiPaneStream,
		},
		{
			Name:    "many-threads",
			Summary: "30 threads with history, then a thread-switch storm",
			seed:    seedManyThreads,
			drive:   driveThreadSwitchStorm,
		},
	}
}

func benchWorkloadByName(name string) (benchWorkload, error) {
	for _, workload := range benchWorkloads() {
		if workload.Name == name {
			return workload, nil
		}
	}
	return benchWorkload{}, usagef("unknown workload %q (want %s)", name, strings.Join(benchWorkloadNames(), ", "))
}

func resolveBenchDuration(workload benchWorkload, requested time.Duration) (time.Duration, error) {
	if requested < 0 {
		return 0, usagef("--duration must not be negative")
	}
	if workload.DefaultDuration == 0 {
		if requested != 0 {
			return 0, usagef("--duration applies only to runner-timed workloads (active-multi-pane)")
		}
		return 0, nil
	}
	resolved := requested
	if resolved == 0 {
		resolved = workload.DefaultDuration
	}
	if workload.MinimumDuration <= 0 {
		return 0, fmt.Errorf("runner-timed workload %s has no positive minimum duration", workload.Name)
	}
	if resolved < workload.MinimumDuration {
		return 0, usagef("--duration for %s must be at least %s", workload.Name, workload.MinimumDuration)
	}
	return resolved, nil
}

// benchGateVerdict is what `--baseline` actually gates on, split out so
// the exit code is testable without a backend.
//
// Two ways to fail, one code. Drift is the obvious one. The other is a
// budgeted metric the run never MEASURED: without it a headless bench (no
// page, so every frontend metric is absent) printed an empty comparison
// table and exited 0 — a run that measured nothing reporting success,
// which is worse than a red, because CI believes it.
func benchGateVerdict(comparisons []benchComparison, unbudgeted []string, baselineFile string) error {
	if n := countDrift(comparisons); n > 0 {
		return exitCodeError{code: exitBadNews, err: fmt.Errorf(
			"%d metric(s) drifted past the baseline in %s", n, baselineFile)}
	}
	if len(unbudgeted) > 0 {
		return exitCodeError{code: exitBadNews, err: fmt.Errorf(
			"%d budgeted metric(s) were not measured this run: %s (a headless run answers no frontend metric — open a page with `make harness-window`)",
			len(unbudgeted), strings.Join(unbudgeted, ", "))}
	}
	return nil
}

func benchWorkloadNames() []string {
	workloads := benchWorkloads()
	names := make([]string, 0, len(workloads))
	for _, workload := range workloads {
		names = append(names, workload.Name)
	}
	return names
}

func countDrift(comparisons []benchComparison) int {
	n := 0
	for _, comparison := range comparisons {
		if comparison.Drift {
			n++
		}
	}
	return n
}

func renderBenchDocument(document benchDocument, path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "bench %s  (%s)\n", document.Workload, document.Description)
	fmt.Fprintf(&b, "  instance %s  version %s  runs %d\n",
		document.Instance, orDash(document.Version), len(document.Runs))
	for _, run := range document.Runs {
		extra := ""
		if run.Switches > 0 {
			extra = fmt.Sprintf("  %d switches over %d threads", run.Switches, run.Threads)
		} else if len(run.Progress) > 0 {
			extra = fmt.Sprintf("  %d visible-progress samples", len(run.Progress))
		}
		fmt.Fprintf(&b, "  run %d: %dms%s\n", run.Run, run.DurationMs, extra)
	}
	b.WriteString("\n")
	names := make([]string, 0, len(document.Aggregate))
	for name := range document.Aggregate {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([][]string, 0, len(names))
	for _, name := range names {
		agg := document.Aggregate[name]
		rows = append(rows, []string{
			name,
			formatBenchValue(agg.P50, agg.Unit),
			formatBenchValue(agg.P95, agg.Unit),
			formatBenchValue(agg.Min, agg.Unit),
			formatBenchValue(agg.Max, agg.Unit),
			fmt.Sprint(agg.Runs),
		})
	}
	b.WriteString(tableString([]string{"METRIC", "P50", "P95", "MIN", "MAX", "RUNS"}, rows))
	if document.Trace != nil {
		b.WriteString("\n")
		b.WriteString(renderTraceSummary(*document.Trace))
	}
	fmt.Fprintf(&b, "\nreport: %s\n", path)
	return b.String()
}

func renderBenchComparison(comparisons []benchComparison, unmeasured, unbudgeted []string, baselinePath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "baseline %s\n", baselinePath)
	if len(comparisons) == 0 {
		b.WriteString("  nothing to compare\n")
	}
	rows := make([][]string, 0, len(comparisons))
	for _, comparison := range comparisons {
		verdict := "ok"
		if comparison.Drift {
			verdict = "DRIFT"
		}
		rows = append(rows, []string{
			comparison.Metric,
			fmt.Sprintf("%.2f", comparison.Current),
			fmt.Sprintf("%.2f", comparison.Reference),
			fmt.Sprintf("%.2f", comparison.Limit),
			comparison.Note,
			verdict,
		})
	}
	b.WriteString(tableString([]string{"METRIC", "CURRENT", "BASELINE", "LIMIT", "RULE", ""}, rows))
	if len(unmeasured) > 0 {
		fmt.Fprintf(&b, "  not measured this run: %s\n", strings.Join(unmeasured, ", "))
	}
	if len(unbudgeted) > 0 {
		fmt.Fprintf(&b, "  FAILED (explicitly budgeted, not measured): %s\n", strings.Join(unbudgeted, ", "))
	}
	return b.String()
}
