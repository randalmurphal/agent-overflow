package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/cdpclient"
	"agent-overflow/internal/harnessclient"
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
	prepare func(ctx context.Context, run *benchRun) error
	drive   func(ctx context.Context, run *benchRun) error
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
			prepare:         openPanesForMultiPaneStream,
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

// benchPerfSpec is what every repeat arms its meters with, carried as one
// value so a new knob is a field rather than another positional threaded
// through three call sites. Zero/empty fields are simply not sent, which
// leaves the backend's and the bridge's own defaults in force.
type benchPerfSpec struct {
	SampleMs  int
	BudgetsMs []float64
	Meters    []string
}

func (s benchPerfSpec) armSpec() map[string]any {
	spec := map[string]any{}
	if s.SampleMs > 0 {
		spec["sampleMs"] = s.SampleMs
	}
	if len(s.BudgetsMs) > 0 {
		spec["budgetsMs"] = s.BudgetsMs
	}
	if len(s.Meters) > 0 {
		spec["meters"] = s.Meters
	}
	return spec
}

// benchRun is the mutable state one repeat carries between its phases.
type benchRun struct {
	env      *env
	client   *harnessclient.Client
	target   target
	workload benchWorkload
	index    int
	duration time.Duration
	// cdp is the attached debugger when --trace is on, nil otherwise. It
	// is the ONE thing in a bench that is not engine-agnostic, which is
	// why it is opt-in rather than part of the run.
	cdp *cdpclient.Conn

	threadIDs []string
	// switches counts the thread opens a storm workload drove, so the
	// report says what the numbers are numbers OF.
	switches int
	// trace is this repeat's forced-layout answer, nil without --trace.
	trace *traceSummary
	// progress is the low-frequency proof that every pane kept rendering
	// new text during a runner-timed workload, not merely holding an active
	// provider timer over a static DOM.
	progress []benchVisibleProgress
}

type benchVisibleProgress struct {
	AtMs        int64          `json:"atMs"`
	TextLengths map[string]int `json:"textLengths"`
	ScrollPx    map[string]int `json:"scrollHeightsPx"`
}

// benchRunTrace is the per-repeat trace headline. The call-site table is
// merged across repeats at the document level rather than repeated per
// run: a reader wants the ranking once, over the whole bench.
type benchRunTrace struct {
	ForcedEvents int     `json:"forcedEvents"`
	ForcedMs     float64 `json:"forcedMs"`
	CallSites    int     `json:"callSites"`
}

// benchRunReport is one repeat's row in the report file.
type benchRunReport struct {
	Run        int                    `json:"run"`
	StartedAt  string                 `json:"startedAt"`
	DurationMs int64                  `json:"durationMs"`
	Threads    int                    `json:"threads,omitempty"`
	Switches   int                    `json:"switches,omitempty"`
	Progress   []benchVisibleProgress `json:"visibleProgress,omitempty"`
	Trace      *benchRunTrace         `json:"trace,omitempty"`
	Perf       perfReport             `json:"perf"`
}

// benchDocument is what lands on disk. It doubles as a baseline: the
// `aggregate` map is exactly what --baseline reads back.
type benchDocument struct {
	Workload            string                    `json:"workload"`
	Description         string                    `json:"description"`
	Scenario            string                    `json:"scenario,omitempty"`
	Repeat              int                       `json:"repeat"`
	StartedAt           string                    `json:"startedAt"`
	Instance            string                    `json:"instance"`
	Version             string                    `json:"version"`
	SampleMs            int                       `json:"sampleMs"`
	RequestedDurationMs int64                     `json:"requestedDurationMs,omitempty"`
	Runs                []benchRunReport          `json:"runs"`
	Aggregate           map[string]benchAggregate `json:"aggregate"`
	// Trace is the forced-layout answer merged over every repeat, present
	// only for a `--trace` run. It is deliberately NOT part of `aggregate`:
	// a baseline compares numbers a headless run can also produce, and a
	// call-site ranking is evidence rather than a metric.
	Trace *traceSummary `json:"trace,omitempty"`
}

func runBench(e *env, args []string) error {
	flags := e.newFlagSet("bench <workload>")
	repeat := flags.Int("repeat", 1, "run the workload this many times and aggregate")
	duration := flags.Duration("duration", 0, "runner-timed workload duration (active-multi-pane defaults to 30s)")
	sampleMs := flags.Int("sample-ms", 0, "perf sampling interval (default 1000, floor 250)")
	budgets := flags.String("budgets", "",
		"comma-separated main-thread budgets in ms for the busy-time fit report (bridge default 6,8,16)")
	var meters stringList
	flags.Var(&meters, "meter",
		"arm only this meter (repeatable: frames, busy, longtask, loaf, layout-shift, event, memory, dom)")
	baselineFile := flags.String("baseline", "", "compare the aggregate against this baseline (a budget file or a previous bench report)")
	outDir := flags.String("out", "", "write the report here instead of <dataDir>/bench")
	asJSON := flags.Bool("json", false, "print the whole report document instead of a summary table")
	trace := flags.Bool("trace", false, "also record a Chromium timeline trace and report the JS call sites that forced layout (needs --cdp)")
	cdp := bindCDPFlag(flags)
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("bench needs exactly one workload: %s", strings.Join(benchWorkloadNames(), ", "))
	}
	if *repeat < 1 {
		return usagef("--repeat must be at least 1")
	}
	workload, err := benchWorkloadByName(rest[0])
	if err != nil {
		return err
	}
	resolvedDuration, err := resolveBenchDuration(workload, *duration)
	if err != nil {
		return err
	}
	budgetsMs, err := parseBudgetsMs(*budgets)
	if err != nil {
		return err
	}
	perfSpec := benchPerfSpec{
		SampleMs:  *sampleMs,
		BudgetsMs: budgetsMs,
		Meters:    []string(meters),
	}
	// --trace is resolved BEFORE anything attaches, for the same reason the
	// bridge is probed before the first reset: a caller who asked for a
	// trace and named no endpoint should get their instance back untouched
	// rather than a bench that ran and answered half the question.
	var traceEndpoint *cdpclient.Endpoint
	if *trace {
		t, err := e.resolveTarget()
		if err != nil {
			return err
		}
		endpoint, err := resolveCDPEndpoint(*cdp, t)
		if err != nil {
			return err
		}
		traceEndpoint = &endpoint
	}
	var baseline *benchBaseline
	if *baselineFile != "" {
		loaded, err := readBenchBaseline(*baselineFile)
		if err != nil {
			return err
		}
		baseline = &loaded
	}

	// A sustained workload owns live provider turns. Ctrl-C must cancel the
	// driver context so its deferred interrupt runs; the process default would
	// exit immediately and leave every mock turn streaming in the backend.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var document benchDocument
	err = e.withClient(ctx, func(client *harnessclient.Client, t target, bs harnessclient.Bootstrap) error {
		document, err = executeBench(ctx, e, client, t, bs, workload, *repeat, resolvedDuration, perfSpec, traceEndpoint)
		return err
	})
	if err != nil {
		return err
	}

	path, err := writeBenchDocument(document, e, *outDir)
	if err != nil {
		return err
	}
	if *asJSON || e.jsonOutput() {
		if err := e.writeJSON(document); err != nil {
			return err
		}
	} else {
		e.printf("%s", renderBenchDocument(document, path))
	}
	if baseline == nil {
		return nil
	}
	comparisons, unmeasured, unbudgeted := compareToBaselineDetailed(document.Aggregate, *baseline)
	e.printf("\n%s", renderBenchComparison(comparisons, unmeasured, unbudgeted, *baselineFile))
	return benchGateVerdict(comparisons, unbudgeted, *baselineFile)
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

func executeBench(
	ctx context.Context,
	e *env,
	client *harnessclient.Client,
	t target,
	bs harnessclient.Bootstrap,
	workload benchWorkload,
	repeat int,
	duration time.Duration,
	perf benchPerfSpec,
	traceEndpoint *cdpclient.Endpoint,
) (benchDocument, error) {
	document := benchDocument{
		Workload:            workload.Name,
		Description:         workload.description(duration),
		Scenario:            workload.Scenario,
		Repeat:              repeat,
		StartedAt:           time.Now().Format(time.RFC3339),
		Instance:            t.ID,
		Version:             bs.Version,
		RequestedDurationMs: duration.Milliseconds(),
	}
	// Fail on the bridge BEFORE resetting anything: a caller who forgot to
	// open a window should get their instance back untouched.
	if err := probeBridge(ctx, e, client); err != nil {
		return document, err
	}
	// And narrow the wire before anything streams: a bench measures the app
	// under a flood, and an unnarrowed CLI connection makes the instrument
	// part of the load.
	if err := narrowBenchSubscription(ctx, client); err != nil {
		return document, err
	}
	// The debugger is attached BEFORE the first reset too, and for the
	// stronger version of the same reason: `Tracing.start` is refused by a
	// browser that is already recording, and an unreachable endpoint
	// discovered after run 1 has already destroyed the instance's state.
	var conn *cdpclient.Conn
	if traceEndpoint != nil {
		attached, page, err := attachCDP(ctx, *traceEndpoint, t, bs)
		if err != nil {
			return document, err
		}
		defer attached.Close()
		conn = attached
		if !e.jsonOutput() {
			e.printf("tracing %s\n", orDash(page.URL))
		}
	}

	traces := make([]traceSummary, 0, repeat)
	reports := make([]perfReport, 0, repeat)
	for i := 1; i <= repeat; i++ {
		run := &benchRun{env: e, client: client, target: t, workload: workload, index: i, duration: duration, cdp: conn}
		startedAt := time.Now()
		e.printf("bench %s: run %d/%d\n", workload.Name, i, repeat)
		report, err := executeBenchRun(ctx, run, perf)
		if err != nil {
			return document, fmt.Errorf("run %d/%d: %w", i, repeat, err)
		}
		// The RESOLVED interval, not the requested one. A default run asks
		// for 0 and the backend picks 1000; recording the request would put
		// a sampleMs of 0 in a document that later serves as a baseline.
		if i == 1 {
			document.SampleMs = report.SampleMs
		}
		row := benchRunReport{
			Run:        i,
			StartedAt:  startedAt.Format(time.RFC3339),
			DurationMs: time.Since(startedAt).Milliseconds(),
			Threads:    len(run.threadIDs),
			Switches:   run.switches,
			Progress:   run.progress,
			Perf:       report,
		}
		if run.trace != nil {
			traces = append(traces, *run.trace)
			row.Trace = &benchRunTrace{
				ForcedEvents: run.trace.ForcedEvents,
				ForcedMs:     run.trace.ForcedMs,
				CallSites:    len(run.trace.Groups),
			}
		}
		document.Runs = append(document.Runs, row)
		reports = append(reports, report)
	}
	document.Aggregate = aggregateBenchMetrics(reports)
	document.Trace = mergeTraceSummaries(traces)
	return document, nil
}

// executeBenchRun is one repeat: blank slate, fixture, armed meters, the
// workload, the report.
func executeBenchRun(ctx context.Context, run *benchRun, perf benchPerfSpec) (perfReport, error) {
	if _, err := run.client.Call(ctx, "HarnessReset"); err != nil {
		return perfReport{}, err
	}
	run.client.Clear()
	if err := run.workload.seedFixture(run); err != nil {
		return perfReport{}, err
	}
	if run.workload.Scenario != "" {
		if _, err := run.client.Call(ctx, "HarnessSetScenario",
			map[string]any{"name": run.workload.Scenario}); err != nil {
			return perfReport{}, err
		}
	}
	// The reload comes AFTER the seed, and both halves of that order are
	// load-bearing. HarnessReset's contract ends with "reload the page
	// after", because the SPA is holding rows that no longer exist. And
	// HarnessSeed writes straight to the store without emitting the
	// creation events a live thread would, so a page reloaded BEFORE the
	// seed never learns the new rows exist and cannot open one. One reload,
	// placed once, answers both.
	if err := reloadPage(ctx, run.env, run.client); err != nil {
		return perfReport{}, err
	}
	if len(run.threadIDs) > 0 {
		if err := openThreadOnPage(ctx, run.env, run.client, run.threadIDs[0]); err != nil {
			return perfReport{}, err
		}
	}
	// Anything else the workload needs ON SCREEN happens here, before the
	// trace and the meters: setup a workload is not about must not be
	// inside the window it is measured in.
	if run.workload.prepare != nil {
		if err := run.workload.prepare(ctx, run); err != nil {
			return perfReport{}, err
		}
	}

	// Tracing starts after the page has been reloaded, settled and pointed
	// at its thread, so the recording covers the WORKLOAD rather than the
	// mount that precedes every repeat.
	var tracing *traceSession
	if run.cdp != nil {
		started, err := startTracing(ctx, run.cdp)
		if err != nil {
			return perfReport{}, err
		}
		tracing = started
	}

	if _, err := run.client.Call(ctx, "HarnessPerfStart", perf.armSpec()); err != nil {
		_, _ = tracing.stop(ctx)
		return perfReport{}, uiQueryError(err)
	}
	driveErr := run.workload.drive(ctx, run)
	// Stop the meters whatever happened: a failed drive still produced
	// numbers, and leaving a run armed would refuse the next repeat.
	raw, stopErr := run.client.Call(ctx, "HarnessPerfStop")
	// And end the recording whatever happened too, for the harder version
	// of the same reason: a browser left recording refuses the next
	// repeat's Tracing.start, so one failed run would fail every run after
	// it with an unrelated message. The trace is read AFTER the meters
	// stop, because draining tens of megabytes over the debugger socket is
	// itself main-thread work on the page being measured.
	traceSummaryValue, traceErr := readTraceSummary(ctx, tracing)
	if driveErr != nil {
		return perfReport{}, driveErr
	}
	if stopErr != nil {
		return perfReport{}, stopErr
	}
	if traceErr != nil {
		return perfReport{}, traceErr
	}
	run.trace = traceSummaryValue
	report, err := decodePerfReport(raw)
	if err != nil {
		return perfReport{}, err
	}
	// The page failing to answer is a FAILED RUN, not a run with fewer
	// columns. Ignoring it produced a report whose frontend half was
	// silently absent — indistinguishable from a headless instance, and
	// folded into an aggregate that a later `--baseline` would compare
	// against as if it were a measurement.
	if report.FrontendError != "" {
		return perfReport{}, fmt.Errorf("the page did not answer the perf meters: %s", report.FrontendError)
	}
	return report, nil
}

func (w benchWorkload) seedFixture(run *benchRun) error {
	raw, err := w.seed(run)
	if err != nil {
		return err
	}
	ctx := context.Background()
	result, err := run.client.CallRaw(ctx, "HarnessSeed", []json.RawMessage{raw})
	if err != nil {
		return err
	}
	var decoded struct {
		Projects []struct {
			ThreadIDs []string `json:"threadIds"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return fmt.Errorf("decode seed result: %w", err)
	}
	for _, project := range decoded.Projects {
		run.threadIDs = append(run.threadIDs, project.ThreadIDs...)
	}
	if len(run.threadIDs) == 0 {
		return errors.New("seed created no threads")
	}
	return nil
}

func writeBenchDocument(document benchDocument, e *env, outDir string) (string, error) {
	dir := outDir
	if dir == "" {
		t, err := e.resolveTarget()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(t.DataDir, benchDirName)
	}
	name := fmt.Sprintf("%s-%s.json", document.Workload, time.Now().Format("20060102-150405"))
	path := filepath.Join(dir, name)
	if err := atomicfile.WriteJSON(path, document); err != nil {
		return "", fmt.Errorf("write bench report: %w", err)
	}
	return path, nil
}

func readBenchBaseline(path string) (benchBaseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return benchBaseline{}, err
	}
	var baseline benchBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return benchBaseline{}, fmt.Errorf("read baseline %s: %w", path, err)
	}
	if len(baseline.Metrics) == 0 && len(baseline.Aggregate) == 0 {
		return benchBaseline{}, fmt.Errorf(
			"baseline %s carries neither `metrics` (a budget) nor `aggregate` (a previous bench report)", path)
	}
	return baseline, nil
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
