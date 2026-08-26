package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/harnessclient"
)

// A bench run is a scripted workload driven against a REAL attached page,
// with the perf meters armed around it. Three things make it a bench
// rather than a soak: it seeds its own fixture, it runs to a completion
// signal instead of forever, and it writes a report a later run can be
// compared against.
//
// WHICH COMPLETION SIGNAL. Two candidates exist, and they answer different
// questions. `harness:mock`'s `scenario_done` says the MOCK finished
// writing its script, which is upstream of everything a bench measures:
// the app has not yet parsed the tail, triaged it, persisted it, or
// rendered it. `provider:turn_completed` is emitted by triage after the
// terminal `result` envelope has been classified and the round closed, so
// it is the first moment the whole pipeline under test is done. That is
// the one this waits on, per thread id. A short settle follows, so the
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
	benchDirName       = "bench"
)

// benchWorkload is one named workload: the fixture it needs and the thing
// it does between arming and stopping the meters.
type benchWorkload struct {
	Name string
	// Scenario is the library entry the mock runs, empty for a workload
	// that drives no provider turn.
	Scenario string
	Summary  string
	seed     func(run *benchRun) (json.RawMessage, error)
	drive    func(ctx context.Context, run *benchRun) error
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

// benchRun is the mutable state one repeat carries between its phases.
type benchRun struct {
	env      *env
	client   *harnessclient.Client
	target   target
	workload benchWorkload
	index    int

	threadIDs []string
	// switches counts the thread opens a storm workload drove, so the
	// report says what the numbers are numbers OF.
	switches int
}

// benchRunReport is one repeat's row in the report file.
type benchRunReport struct {
	Run        int        `json:"run"`
	StartedAt  string     `json:"startedAt"`
	DurationMs int64      `json:"durationMs"`
	Threads    int        `json:"threads,omitempty"`
	Switches   int        `json:"switches,omitempty"`
	Perf       perfReport `json:"perf"`
}

// benchDocument is what lands on disk. It doubles as a baseline: the
// `aggregate` map is exactly what --baseline reads back.
type benchDocument struct {
	Workload    string                    `json:"workload"`
	Description string                    `json:"description"`
	Scenario    string                    `json:"scenario,omitempty"`
	Repeat      int                       `json:"repeat"`
	StartedAt   string                    `json:"startedAt"`
	Instance    string                    `json:"instance"`
	Version     string                    `json:"version"`
	SampleMs    int                       `json:"sampleMs"`
	Runs        []benchRunReport          `json:"runs"`
	Aggregate   map[string]benchAggregate `json:"aggregate"`
}

func runBench(e *env, args []string) error {
	flags := e.newFlagSet("bench <workload>")
	repeat := flags.Int("repeat", 1, "run the workload this many times and aggregate")
	sampleMs := flags.Int("sample-ms", 0, "perf sampling interval (default 1000, floor 250)")
	baselineFile := flags.String("baseline", "", "compare the aggregate against this baseline (a budget file or a previous bench report)")
	outDir := flags.String("out", "", "write the report here instead of <dataDir>/bench")
	asJSON := flags.Bool("json", false, "print the whole report document instead of a summary table")
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
	var baseline *benchBaseline
	if *baselineFile != "" {
		loaded, err := readBenchBaseline(*baselineFile)
		if err != nil {
			return err
		}
		baseline = &loaded
	}

	ctx := context.Background()
	var document benchDocument
	err = e.withClient(ctx, func(client *harnessclient.Client, t target, bs harnessclient.Bootstrap) error {
		document, err = executeBench(ctx, e, client, t, bs, workload, *repeat, *sampleMs)
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
	repeat, sampleMs int,
) (benchDocument, error) {
	document := benchDocument{
		Workload:    workload.Name,
		Description: workload.Summary,
		Scenario:    workload.Scenario,
		Repeat:      repeat,
		StartedAt:   time.Now().Format(time.RFC3339),
		Instance:    t.ID,
		Version:     bs.Version,
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

	reports := make([]perfReport, 0, repeat)
	for i := 1; i <= repeat; i++ {
		run := &benchRun{env: e, client: client, target: t, workload: workload, index: i}
		startedAt := time.Now()
		e.printf("bench %s: run %d/%d\n", workload.Name, i, repeat)
		report, err := executeBenchRun(ctx, run, sampleMs)
		if err != nil {
			return document, fmt.Errorf("run %d/%d: %w", i, repeat, err)
		}
		// The RESOLVED interval, not the requested one. A default run asks
		// for 0 and the backend picks 1000; recording the request would put
		// a sampleMs of 0 in a document that later serves as a baseline.
		if i == 1 {
			document.SampleMs = report.SampleMs
		}
		document.Runs = append(document.Runs, benchRunReport{
			Run:        i,
			StartedAt:  startedAt.Format(time.RFC3339),
			DurationMs: time.Since(startedAt).Milliseconds(),
			Threads:    len(run.threadIDs),
			Switches:   run.switches,
			Perf:       report,
		})
		reports = append(reports, report)
	}
	document.Aggregate = aggregateBenchMetrics(reports)
	return document, nil
}

// executeBenchRun is one repeat: blank slate, fixture, armed meters, the
// workload, the report.
func executeBenchRun(ctx context.Context, run *benchRun, sampleMs int) (perfReport, error) {
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

	spec := map[string]any{}
	if sampleMs > 0 {
		spec["sampleMs"] = sampleMs
	}
	if _, err := run.client.Call(ctx, "HarnessPerfStart", spec); err != nil {
		return perfReport{}, uiQueryError(err)
	}
	driveErr := run.workload.drive(ctx, run)
	// Stop the meters whatever happened: a failed drive still produced
	// numbers, and leaving a run armed would refuse the next repeat.
	raw, stopErr := run.client.Call(ctx, "HarnessPerfStop")
	if driveErr != nil {
		return perfReport{}, driveErr
	}
	if stopErr != nil {
		return perfReport{}, stopErr
	}
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
