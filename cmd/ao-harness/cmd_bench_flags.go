package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/cdpclient"
	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/harnessrun"
)

// benchPerfSpec is the perf and monitor configuration every repeat arms.
// Carrying it as one value keeps a new measurement knob local to the bench
// option surface rather than threading another argument through each phase.
type benchPerfSpec struct {
	SampleMs   int
	BudgetsMs  []float64
	Meters     []string
	Monitors   []string
	MonitorLeg string
	PageID     string
}

// benchCLIOptions owns the flag pointers for one bench invocation. Keeping
// construction in one place lets `bench -h` print the same surface without
// resolving an instance or creating a run manifest first.
type benchCLIOptions struct {
	repeat       *int
	duration     *time.Duration
	sampleMs     *int
	budgets      *string
	meters       *stringList
	monitors     *stringList
	monitorLeg   *string
	baselineFile *string
	outDir       *string
	asJSON       *bool
	trace        *bool
	leg          *string
	cdp          *string
}

func newBenchFlagSet(e *env) (*flag.FlagSet, *benchCLIOptions) {
	flags := e.newFlagSet("bench <workload>")
	opts := &benchCLIOptions{meters: new(stringList), monitors: new(stringList)}
	opts.repeat = flags.Int("repeat", 1, "run the workload this many times and aggregate")
	opts.duration = flags.Duration("duration", 0, "runner-timed workload duration (active-multi-pane defaults to 30s)")
	opts.sampleMs = flags.Int("sample-ms", 0, "perf sampling interval (default 1000, floor 250)")
	opts.budgets = flags.String("budgets", "",
		"comma-separated main-thread budgets in ms for the busy-time fit report (bridge default 6,8,16)")
	flags.Var(opts.meters, "meter",
		"arm only this meter (repeatable: frames, busy, longtask, loaf, layout-shift, event, memory, dom)")
	flags.Var(opts.monitors, "monitor", "arm a typed app-feel monitor (repeatable; persisted in the perf and bench report)")
	opts.monitorLeg = flags.String("monitor-leg", "", "compatibility leg required by selected app-feel monitors")
	opts.baselineFile = flags.String("baseline", "", "compare the aggregate against this baseline (a budget file or a previous bench report)")
	opts.outDir = flags.String("out", "", "write the report here instead of <dataDir>/bench")
	opts.asJSON = flags.Bool("json", false, "print the whole report document instead of a summary table")
	opts.trace = flags.Bool("trace", false, "also record a Chromium timeline trace and report the JS call sites that forced layout (needs --cdp)")
	opts.leg = flags.String("leg", "", "measurement leg (clean-memory, frame-cpu, cpu-profile, allocation, trace, correctness; inferred from --trace when omitted)")
	opts.cdp = bindCDPFlag(flags)
	return flags, opts
}

func parseBenchOptions(e *env, args []string) (*benchCLIOptions, []string, error) {
	flags, opts := newBenchFlagSet(e)
	rest, err := e.parse(flags, args)
	return opts, rest, err
}

func validateBenchInvocation(opts *benchCLIOptions, rest []string) (benchWorkload, time.Duration, []float64, string, error) {
	if len(rest) != 1 {
		return benchWorkload{}, 0, nil, "", usagef("bench needs exactly one workload: %s", strings.Join(benchWorkloadNames(), ", "))
	}
	if *opts.repeat < 1 {
		return benchWorkload{}, 0, nil, "", usagef("--repeat must be at least 1")
	}
	workload, err := benchWorkloadByName(rest[0])
	if err != nil {
		return benchWorkload{}, 0, nil, "", err
	}
	resolvedDuration, err := resolveBenchDuration(workload, *opts.duration)
	if err != nil {
		return benchWorkload{}, 0, nil, "", err
	}
	budgetsMs, err := parseBudgetsMs(*opts.budgets)
	if err != nil {
		return benchWorkload{}, 0, nil, "", err
	}
	resolvedLeg := strings.TrimSpace(*opts.leg)
	if resolvedLeg == "" {
		resolvedLeg = benchLegCorrectness
		if *opts.trace {
			resolvedLeg = benchLegTrace
		}
	}
	if resolvedLeg == benchLegCleanMemory && (*opts.trace || len(*opts.monitors) > 0 || len(*opts.meters) > 0 || len(budgetsMs) > 0) {
		return benchWorkload{}, 0, nil, "", usagef("%s leg cannot use frontend meters, budgets, monitors, or trace", benchLegCleanMemory)
	}
	if resolvedLeg == benchLegCPUProfile || resolvedLeg == benchLegAllocation {
		return benchWorkload{}, 0, nil, "", usagef("%s is not implemented by `ao-harness bench`; use `ao-harness profile` for CPU profiles, and use an external allocator profiler for allocation runs", resolvedLeg)
	}
	return workload, resolvedDuration, budgetsMs, resolvedLeg, nil
}

func monitorQueryRequirement(monitors []string) []string {
	if len(monitors) == 0 {
		return nil
	}
	return []string{"monitor"}
}

func (s benchPerfSpec) armSpec() map[string]any {
	spec := map[string]any{}
	if s.SampleMs > 0 {
		spec["sampleMs"] = s.SampleMs
	}
	if len(s.BudgetsMs) > 0 {
		spec["budgetsMs"] = s.BudgetsMs
	}
	if s.Meters != nil {
		spec["meters"] = s.Meters
	}
	if len(s.Monitors) > 0 {
		spec["monitors"] = s.Monitors
	}
	if s.MonitorLeg != "" {
		spec["compatibilityLeg"] = s.MonitorLeg
	}
	if s.PageID != "" {
		spec["pageId"] = s.PageID
	}
	return spec
}

func runBench(e *env, args []string) error {
	opts, rest, err := parseBenchOptions(e, args)
	if err != nil {
		return err
	}
	workload, resolvedDuration, budgetsMS, resolvedLeg, err := validateBenchInvocation(opts, rest)
	if err != nil {
		return err
	}
	if *opts.trace && strings.TrimSpace(*opts.cdp) == "" {
		if _, err := resolveCDPEndpoint("", target{}); err != nil {
			return err
		}
	}
	// The ordinary bench command uses the same lease and durable lifecycle as
	// `run --plan`. Resolve the target before publishing the manifest so a
	// default-instance fallback cannot mutate a different harness later.
	t, err := e.resolveTarget()
	if err != nil {
		return err
	}
	if t.Row == nil || t.Row.Stale {
		return fmt.Errorf("bench requires a live harness instance at %s", t.DataRoot)
	}
	borrowedBootstrap, err := bootstrapForTarget(t)
	if err != nil {
		return fmt.Errorf("read borrowed instance identity: %w", err)
	}
	plan := harnessrun.ApplyDefaults(harnessrun.RunPlan{
		Version: harnessrun.PlanVersion, RunID: fmt.Sprintf("bench-%d", time.Now().UnixNano()),
		Workload: workload.Name, DataRoot: t.DataRoot, Ownership: harnessrun.OwnershipBorrowed,
		Adapter: harnessrun.AdapterBench, Repeat: *opts.repeat,
		DurationMS: resolvedDuration.Milliseconds(), SampleMS: *opts.sampleMs,
		BudgetsMS: append([]float64(nil), budgetsMS...), Meters: append([]string(nil), (*opts.meters)...),
		Monitors: append([]string(nil), (*opts.monitors)...), MonitorLeg: strings.TrimSpace(*opts.monitorLeg),
		Leg: resolvedLeg, Trace: *opts.trace, CDP: strings.TrimSpace(*opts.cdp),
	})
	retention, err := harnessrun.NewDefaultArtifactRegistry()
	if err != nil {
		return err
	}
	sup, err := harnessrun.NewWithOptions(plan, time.Now().UTC(), harnessrun.SupervisorOptions{Retention: retention})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ownerPID := os.Getpid()
	if t.Row != nil {
		ownerPID = t.Row.PID
	}
	var borrowedShutdownDone chan struct{}
	var borrowedShutdownErr chan error
	var borrowedShutdownOnce sync.Once
	onSafety := func() { cancel() }
	if plan.Ownership == harnessrun.OwnershipBorrowed {
		borrowedShutdownDone = make(chan struct{})
		borrowedShutdownErr = make(chan error, 1)
		onSafety = func() {
			cancel()
			borrowedShutdownOnce.Do(func() {
				shutdownErr := stopVictim(context.Background(), borrowedBootstrap.PID, borrowedBootstrap, true)
				borrowedShutdownErr <- shutdownErr
				close(borrowedShutdownDone)
			})
		}
	}
	reservation, err := startRunGovernor(ctx, plan, ownerPID, onSafety)
	if err != nil {
		return finishManagedBench(sup, ctx, err, harnessrun.FailureSafetyCeiling)
	}
	for _, step := range []struct {
		state harnessrun.State
		phase harnessrun.Phase
	}{{harnessrun.StatePreparing, harnessrun.PhasePrepare}, {harnessrun.StateReady, harnessrun.PhaseReady}, {harnessrun.StateRunning, harnessrun.PhaseAction}} {
		if err := sup.Transition(step.state, step.phase); err != nil {
			return finishManagedBenchWithCleanup(sup, ctx, err, harnessrun.FailureReadiness, reservation.cleanup)
		}
	}
	directEnv := *e
	directEnv.instance = t.DataRoot
	actionErr := runBenchDirectContext(ctx, &directEnv, args)
	class := harnessrun.FailureNone
	if safetyErr := reservation.safetyError(); safetyErr != nil {
		actionErr = safetyErr
		class = harnessrun.FailureSafetyCeiling
		if borrowedShutdownDone != nil {
			select {
			case <-borrowedShutdownDone:
			case <-time.After(20 * time.Second):
				actionErr = errors.Join(actionErr, errors.New("borrowed harness shutdown did not complete after safety event"))
			}
			select {
			case shutdownErr := <-borrowedShutdownErr:
				if shutdownErr != nil {
					actionErr = errors.Join(actionErr, fmt.Errorf("shut down borrowed harness after safety event: %w", shutdownErr))
				}
			default:
			}
		}
	}
	if actionErr != nil && class == harnessrun.FailureNone {
		class = harnessrun.FailureAction
	}
	finishErr := sup.Finish(ctx, actionErr, class, reservation.cleanup)
	if finishErr != nil {
		return finishErr
	}
	return actionErr
}

func finishManagedBench(sup *harnessrun.Supervisor, ctx context.Context, cause error, class harnessrun.FailureClass) error {
	if err := sup.Finish(ctx, cause, class, nil); err != nil {
		return err
	}
	return cause
}

func finishManagedBenchWithCleanup(sup *harnessrun.Supervisor, ctx context.Context, cause error, class harnessrun.FailureClass, cleanup func(context.Context) error) error {
	if err := sup.Finish(ctx, cause, class, cleanup); err != nil {
		return err
	}
	return cause
}

func runBenchDirect(e *env, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runBenchDirectContext(ctx, e, args)
}

func runBenchDirectContext(ctx context.Context, e *env, args []string) error {
	opts, rest, err := parseBenchOptions(e, args)
	if err != nil {
		return err
	}
	workload, resolvedDuration, budgetsMs, resolvedLeg, err := validateBenchInvocation(opts, rest)
	if err != nil {
		return err
	}
	// `--json` is an output mode, not only a final rendering choice. Set the
	// invocation format before any run progress is emitted so stdout remains
	// parseable JSON from the first byte.
	if *opts.asJSON {
		e.format = "json"
	}
	perfSpec := benchPerfSpec{
		SampleMs:   *opts.sampleMs,
		BudgetsMs:  budgetsMs,
		Meters:     []string(*opts.meters),
		Monitors:   []string(*opts.monitors),
		MonitorLeg: strings.TrimSpace(*opts.monitorLeg),
		PageID:     strings.TrimSpace(e.pageID),
	}
	if resolvedLeg == benchLegCleanMemory {
		// A non-nil empty list is distinct from omitted meters. It arms only
		// the backend sampler and leaves the page with no perf observers, rAF
		// loop, DOM census, or memory meter.
		perfSpec.Meters = []string{}
	}
	if err := validateBenchLeg(resolvedLeg, benchInstrumentKind(perfSpec, *opts.trace)); err != nil {
		return usagef("%v", err)
	}
	// --trace is resolved BEFORE anything attaches, for the same reason the
	// bridge is probed before the first reset: a caller who asked for a
	// trace and named no endpoint should get their instance back untouched
	// rather than a bench that ran and answered half the question.
	var traceEndpoint *cdpclient.Endpoint
	if *opts.trace {
		t, err := e.resolveTarget()
		if err != nil {
			return err
		}
		endpoint, err := resolveCDPEndpoint(*opts.cdp, t)
		if err != nil {
			return err
		}
		traceEndpoint = &endpoint
	}
	var baseline *benchBaseline
	if *opts.baselineFile != "" {
		loaded, err := readBenchBaseline(*opts.baselineFile)
		if err != nil {
			return err
		}
		baseline = &loaded
	}

	// A sustained workload owns live provider turns. Ctrl-C must cancel the
	// driver context so its deferred interrupt runs; the process default would
	// exit immediately and leave every mock turn streaming in the backend.
	var document benchDocument
	var benchTarget target
	err = e.withClient(ctx, func(client *harnessclient.Client, t target, bs harnessclient.Bootstrap) error {
		benchTarget = t
		if err := requireHarnessProtocol(client, capabilityRequirements{
			Methods: []string{"HarnessReset", "HarnessSeed", "HarnessSetScenario", "HarnessPerfStart", "HarnessPerfStop", "HarnessUIQuery", "HarnessEmit", "SendMessage"},
			Actions: []string{"open", "reload"}, Queries: append([]string{"element", "open", "perf", "viewport"}, monitorQueryRequirement(perfSpec.Monitors)...),
			Workloads: []string{workload.Name}, Meters: perfSpec.Meters,
		}); err != nil {
			return err
		}
		if baseline != nil {
			info, infoErr := client.Info(ctx)
			if infoErr != nil {
				return fmt.Errorf("read benchmark identity: %w", infoErr)
			}
			caps, capsErr := client.CachedCapabilities()
			if capsErr != nil {
				return fmt.Errorf("read benchmark capabilities: %w", capsErr)
			}
			expected := benchReportIdentityFor(bs, caps, info, workload, perfSpec, resolvedDuration, traceEndpoint != nil, resolvedLeg)
			if err := ensureBenchBaselineMatches(expected, *baseline); err != nil {
				return err
			}
		}
		caps, capsErr := client.CachedCapabilities()
		if capsErr != nil {
			return fmt.Errorf("read benchmark capabilities: %w", capsErr)
		}
		info, infoErr := client.Info(ctx)
		if infoErr != nil {
			return fmt.Errorf("read benchmark identity: %w", infoErr)
		}
		identity := benchReportIdentityFor(bs, caps, info, workload, perfSpec, resolvedDuration, traceEndpoint != nil, resolvedLeg)
		document, err = executeBench(ctx, e, client, t, bs, workload, *opts.repeat, resolvedDuration, perfSpec, traceEndpoint, identity, *opts.outDir)
		return err
	})
	if err != nil {
		return err
	}

	// The execution checkpoint intentionally remained `running` until every
	// repeat completed. Only the final artifact write earns a success status.
	document.Status = "succeeded"
	path, err := writeBenchDocument(document, benchTarget, *opts.outDir)
	if err != nil {
		document.Status = "failed"
		document.FailurePhase = "finalize"
		document.FailureClass = "artifact"
		checkpoint := benchCheckpointPath(benchTarget, workload.Name, *opts.outDir)
		if checkpointErr := atomicfile.WriteJSON(checkpoint, document); checkpointErr != nil {
			err = errors.Join(err, fmt.Errorf("write failed bench checkpoint: %w", checkpointErr))
		}
		return err
	}
	checkpoint := benchCheckpointPath(benchTarget, workload.Name, *opts.outDir)
	if err := atomicfile.WriteJSON(checkpoint, document); err != nil {
		document.Status = "failed"
		document.FailurePhase = "finalize"
		document.FailureClass = "artifact"
		return fmt.Errorf("write final bench checkpoint: %w", err)
	}
	if *opts.asJSON || e.jsonOutput() {
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
	e.progressf("\n%s", renderBenchComparison(comparisons, unmeasured, unbudgeted, *opts.baselineFile))
	return benchGateVerdict(comparisons, unbudgeted, *opts.baselineFile)
}
