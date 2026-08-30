package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/cdpclient"
	"agent-overflow/internal/harnessclient"
)

// benchRun is the mutable state one repeat carries between its phases.
type benchRun struct {
	env      *env
	client   *harnessclient.Client
	target   target
	workload benchWorkload
	index    int
	duration time.Duration
	// cdp is the attached debugger when --trace is on, nil otherwise.
	cdp           *cdpclient.Conn
	pageMarker    string
	traceEndpoint *cdpclient.Endpoint
	bootstrap     harnessclient.Bootstrap

	threadIDs         []string
	startedThreadIDs  []string
	switches          int
	trace             *traceSummary
	progress          []benchVisibleProgress
	validity          benchValidityReceipt
	evidence          benchEvidenceCursors
	sourceStartedAt   time.Time
	drain             revealDrain
	drainObserved     bool
	progressInterval  time.Duration
	activeIDs         []string
	activeCompletions chan benchTurnCompletion
	activeWaitCancel  context.CancelFunc
}

type benchVisibleProgress struct {
	AtMs        int64          `json:"atMs"`
	TextLengths map[string]int `json:"textLengths"`
	ScrollPx    map[string]int `json:"scrollHeightsPx"`
}

type benchRunTrace struct {
	ForcedEvents int     `json:"forcedEvents"`
	ForcedMs     float64 `json:"forcedMs"`
	CallSites    int     `json:"callSites"`
}

type benchRunReport struct {
	Run        int                    `json:"run"`
	StartedAt  string                 `json:"startedAt"`
	DurationMs int64                  `json:"durationMs"`
	Threads    int                    `json:"threads,omitempty"`
	Switches   int                    `json:"switches,omitempty"`
	Progress   []benchVisibleProgress `json:"visibleProgress,omitempty"`
	Trace      *benchRunTrace         `json:"trace,omitempty"`
	Validity   benchValidityReceipt   `json:"validity"`
	Perf       perfReport             `json:"perf"`
	Monitors   []string               `json:"monitors,omitempty"`
	MonitorLeg string                 `json:"monitorLeg,omitempty"`
}

type benchDocument struct {
	benchReportIdentity
	Workload            string                    `json:"workload"`
	Description         string                    `json:"description"`
	Scenario            string                    `json:"scenario,omitempty"`
	Repeat              int                       `json:"repeat"`
	StartedAt           string                    `json:"startedAt"`
	Instance            string                    `json:"instance"`
	Version             string                    `json:"version"`
	Monitors            []string                  `json:"monitors,omitempty"`
	MonitorLeg          string                    `json:"monitorLeg,omitempty"`
	SampleMs            int                       `json:"sampleMs"`
	RequestedDurationMs int64                     `json:"requestedDurationMs,omitempty"`
	Status              string                    `json:"status"`
	FailurePhase        string                    `json:"failurePhase,omitempty"`
	FailureClass        string                    `json:"failureClass,omitempty"`
	CompletedRepeats    int                       `json:"completedRepeats"`
	Runs                []benchRunReport          `json:"runs"`
	Aggregate           map[string]benchAggregate `json:"aggregate"`
	Trace               *traceSummary             `json:"trace,omitempty"`
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
	identity benchReportIdentity,
	outDir string,
) (document benchDocument, err error) {
	document = benchDocument{
		benchReportIdentity: identity,
		Workload:            workload.Name,
		Description:         workload.description(duration),
		Scenario:            workload.Scenario,
		Repeat:              repeat,
		StartedAt:           time.Now().Format(time.RFC3339),
		Instance:            t.ID,
		Version:             bs.Version,
		Monitors:            append([]string(nil), perf.Monitors...),
		MonitorLeg:          perf.MonitorLeg,
		RequestedDurationMs: duration.Milliseconds(),
		Status:              "running",
	}
	checkpoint := benchCheckpointPath(t, workload.Name, outDir)
	// Publish a parseable running document before any reset/seed/trace action.
	// This is a recovery receipt, not the final report path.
	if err := atomicfile.WriteJSON(checkpoint, document); err != nil {
		return document, fmt.Errorf("write bench checkpoint: %w", err)
	}
	defer func() {
		if err != nil && document.Status == "running" {
			document.Status = "failed"
			if document.FailurePhase == "" {
				document.FailurePhase = "action"
			}
			if document.FailureClass == "" {
				document.FailureClass = classifyBenchFailure(err)
			}
		}
		if checkpointErr := atomicfile.WriteJSON(checkpoint, document); checkpointErr != nil && err == nil {
			err = fmt.Errorf("write final bench checkpoint: %w", checkpointErr)
		}
	}()
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
	if traceEndpoint != nil {
		// Validate the endpoint and exact page ownership before reset mutates
		// the instance. Each run attaches again after reload because reload
		// mints a new debugger target.
		attached, page, attachErr := attachCDP(ctx, *traceEndpoint, t, bs, e.pageID)
		if attachErr != nil {
			return document, attachErr
		}
		_ = attached.Close()
		e.progressf("tracing %s\n", orDash(page.URL))
	}
	traces := make([]traceSummary, 0, repeat)
	reports := make([]perfReport, 0, repeat)
	for i := 1; i <= repeat; i++ {
		run := &benchRun{env: e, client: client, target: t, workload: workload, index: i, duration: duration, pageMarker: bs.PageMarker, traceEndpoint: traceEndpoint, bootstrap: bs}
		startedAt := time.Now()
		e.progressf("bench %s: run %d/%d\n", workload.Name, i, repeat)
		// Keep an initial cursor for failures during reset, seed or setup. A
		// successful run replaces it after setup, immediately before the
		// measured action, so setup output never contaminates its receipt.
		if info, infoErr := client.Info(ctx); infoErr != nil {
			run.validity = benchValidityReceipt{V: 1, Reasons: []string{fmt.Sprintf("read benchmark evidence paths: %v", infoErr)}}
		} else {
			run.evidence = captureBenchEvidenceCursors(info)
		}
		report, err := executeBenchRun(ctx, run, perf)
		if err != nil {
			document.FailurePhase = "action"
			// A failure before the measured action still needs a complete
			// receipt. Re-fold the run-scoped cursors so setup faults, sequence
			// gaps and the partial drain state survive in the checkpoint.
			if validityErr := finalizeBenchValidity(run); validityErr != nil {
				err = errors.Join(err, validityErr)
			}
			if cleanupErr := cleanupBenchTurns(run); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
			// Keep the attempted repeat and its validity evidence in the
			// checkpoint. The primary run error still returns to the caller,
			// but the durable document explains what was observed before it.
			partial := benchRunReport{
				Run: i, StartedAt: startedAt.Format(time.RFC3339),
				DurationMs: time.Since(startedAt).Milliseconds(),
				Threads:    len(run.threadIDs), Switches: run.switches,
				Progress: run.progress, Validity: run.validity, Perf: report,
				Monitors: append([]string(nil), perf.Monitors...), MonitorLeg: perf.MonitorLeg,
			}
			if run.trace != nil {
				partial.Trace = &benchRunTrace{ForcedEvents: run.trace.ForcedEvents, ForcedMs: run.trace.ForcedMs, CallSites: len(run.trace.Groups)}
			}
			document.Runs = append(document.Runs, partial)
			if checkpointErr := atomicfile.WriteJSON(checkpoint, document); checkpointErr != nil {
				err = errors.Join(err, fmt.Errorf("write partial bench checkpoint: %w", checkpointErr))
			}
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
			Validity:   run.validity,
			Perf:       report,
			Monitors:   append([]string(nil), perf.Monitors...),
			MonitorLeg: perf.MonitorLeg,
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
		document.CompletedRepeats = len(document.Runs)
		if err := atomicfile.WriteJSON(checkpoint, document); err != nil {
			document.FailurePhase = "finalize"
			return document, fmt.Errorf("write bench checkpoint after run %d: %w", i, err)
		}
	}
	document.Aggregate = aggregateBenchMetrics(reports)
	document.Trace = mergeTraceSummaries(traces)
	return document, nil
}

func benchCheckpointPath(t target, workload, outDir string) string {
	dir := outDir
	if dir == "" {
		dir = filepath.Join(t.DataDir, benchDirName)
	} else if benchOutputIsFile(dir) {
		// Managed plans name an exact report file. The checkpoint sits beside
		// it so a failed run never turns the report path into a directory.
		dir = filepath.Dir(dir)
	}
	return filepath.Join(dir, workload+"-checkpoint.json")
}

func benchOutputIsFile(path string) bool {
	return strings.EqualFold(filepath.Ext(strings.TrimSpace(path)), ".json")
}

func classifyBenchFailure(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return "action"
}

func cleanupBenchTurns(run *benchRun) error {
	if run == nil {
		return nil
	}
	if run.activeWaitCancel != nil {
		run.activeWaitCancel()
		run.activeWaitCancel = nil
	}
	if len(run.startedThreadIDs) == 0 {
		return nil
	}
	// Cleanup must outlive a cancelled action context. It still has a hard
	// bound so a wedged provider cannot hold the CLI forever.
	ctx, cancel := context.WithTimeout(context.Background(), benchInterruptTimeout)
	defer cancel()
	if err := interruptBenchTurns(ctx, run.client, run.startedThreadIDs); err != nil {
		return fmt.Errorf("clean up bench turns: %w", err)
	}
	return nil
}

func executeBenchRun(ctx context.Context, run *benchRun, perf benchPerfSpec) (perfReport, error) {
	if _, err := run.client.Call(ctx, "HarnessReset"); err != nil {
		return perfReport{}, err
	}
	run.client.Clear()
	run.client.ClearSequenceObservations()
	if err := run.workload.seedFixture(ctx, run); err != nil {
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
	if _, err := reloadPage(ctx, run.env, run.client, run.pageMarker); err != nil {
		return perfReport{}, err
	}
	perf.PageID = run.env.pageID
	if run.traceEndpoint != nil {
		endpoint, err := run.traceEndpoint.ForRediscovery()
		if err != nil {
			return perfReport{}, fmt.Errorf("prepare trace rediscovery: %w", err)
		}
		conn, page, err := attachCDP(ctx, endpoint, run.target, run.bootstrap, run.env.pageID)
		if err != nil {
			return perfReport{}, err
		}
		run.cdp = conn
		defer conn.Close()
		run.env.progressf("tracing %s\n", orDash(page.URL))
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
	// Evidence cursors are taken after setup and before the measured action.
	// This excludes stale renderer faults and trace records while retaining
	// every record emitted by the workload itself.
	info, err := run.client.Info(ctx)
	if err != nil {
		return perfReport{}, fmt.Errorf("read benchmark evidence paths: %w", err)
	}
	run.evidence = captureBenchEvidenceCursors(info)
	// Setup may have emitted provider traffic. Start both the retained event
	// log and its sequence oracle at the measured action, otherwise a gap from
	// reset/seed or an earlier repeat can poison this run's receipt.
	run.client.Clear()
	run.client.ClearSequenceObservations()
	if run.workload.beforeStart != nil {
		if err := run.workload.beforeStart(ctx, run); err != nil {
			return perfReport{}, err
		}
	}
	if perf.Meters != nil && len(perf.Meters) == 0 {
		if _, err := run.env.queryUI(ctx, run.client, map[string]any{"kind": "perf", "op": "disarm"}); err != nil {
			return perfReport{}, fmt.Errorf("clear clean-leg page observer: %w", err)
		}
	}

	if _, err := run.client.Call(ctx, "HarnessPerfStart", perf.armSpec()); err != nil {
		return perfReport{}, uiQueryError(err)
	}
	// Tracing starts after the page has been reloaded, settled, and the perf
	// arm has completed. The recording therefore covers the WORKLOAD rather
	// than setup or observer installation before every repeat.
	var tracing *traceSession
	if run.cdp != nil {
		started, err := startTracing(ctx, run.cdp)
		if err != nil {
			_, _ = run.client.Call(ctx, "HarnessPerfStop")
			return perfReport{}, err
		}
		tracing = started
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
	run.trace = traceSummaryValue
	validityErr := finalizeBenchValidity(run)
	var report perfReport
	var reportErr error
	if stopErr == nil {
		report, reportErr = decodePerfReport(raw)
		if reportErr != nil {
			invalidateBenchRun(run, "perf report", reportErr)
		}
	}
	if driveErr != nil {
		invalidateBenchRun(run, "drive", driveErr)
		return report, errors.Join(driveErr, validityErr, reportErr)
	}
	if stopErr != nil {
		invalidateBenchRun(run, "perf stop", stopErr)
		return report, errors.Join(stopErr, validityErr, reportErr)
	}
	if traceErr != nil {
		invalidateBenchRun(run, "trace", traceErr)
		return report, errors.Join(traceErr, validityErr, reportErr)
	}
	if validityErr != nil {
		return report, errors.Join(validityErr, reportErr)
	}
	if reportErr != nil {
		return report, reportErr
	}
	// The page failing to answer is a FAILED RUN, not a run with fewer
	// columns. Ignoring it produced a report whose frontend half was
	// silently absent — indistinguishable from a headless instance, and
	// folded into an aggregate that a later `--baseline` would compare
	// against as if it were a measurement.
	if report.FrontendError != "" {
		err := fmt.Errorf("the page did not answer the perf meters: %s", report.FrontendError)
		invalidateBenchRun(run, "frontend perf collection", err)
		return report, err
	}
	if report.MonitorsError != "" {
		err := fmt.Errorf("the page did not complete typed monitors: %s", report.MonitorsError)
		invalidateBenchRun(run, "frontend monitors", err)
		return report, err
	}
	return report, nil
}

func (w benchWorkload) seedFixture(ctx context.Context, run *benchRun) error {
	raw, err := w.seed(run)
	if err != nil {
		return err
	}
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

func writeBenchDocument(document benchDocument, selected target, outDir string) (string, error) {
	if benchOutputIsFile(outDir) {
		if err := atomicfile.WriteJSON(outDir, document); err != nil {
			return "", fmt.Errorf("write bench report: %w", err)
		}
		return outDir, nil
	}
	dir := outDir
	if dir == "" {
		if selected.DataDir == "" {
			return "", errors.New("write bench report: selected target has no data directory")
		}
		dir = filepath.Join(selected.DataDir, benchDirName)
	}
	name := fmt.Sprintf("%s-%s.json", document.Workload, time.Now().Format("20060102-150405"))
	path := filepath.Join(dir, name)
	if err := atomicfile.WriteJSON(path, document); err != nil {
		return "", fmt.Errorf("write bench report: %w", err)
	}
	return path, nil
}
