package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/harnessrun"
)

// run is the managed entry point for repeatable workloads. Its input is a
// strict typed plan, so it cannot turn into an arbitrary RPC or shell door.
func runManaged(e *env, args []string) error {
	flags := e.newFlagSet("run --plan <file|->")
	planPath := flags.String("plan", "", "strict JSON run plan, or - for stdin")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("run takes no positional arguments")
	}
	if strings.TrimSpace(*planPath) == "" {
		return usagef("run needs --plan <file|->")
	}
	data, err := readRunPlan(*planPath)
	if err != nil {
		return err
	}
	plan, err := harnessrun.DecodePlan(data)
	if err != nil {
		return err
	}
	plan = harnessrun.ApplyDefaults(plan)
	e.pageID = strings.TrimSpace(plan.PageID)
	if plan.Instance != "" {
		e.instance = plan.Instance
	} else {
		e.instance = plan.DataRoot
	}
	if plan.Ownership == harnessrun.OwnershipBorrowed && plan.Instance == "" {
		return errors.New("borrowed run requires an explicit instance")
	}
	var selectedTarget *target
	if plan.Ownership == harnessrun.OwnershipBorrowed {
		if plan.Adapter == harnessrun.AdapterFunctional {
			return errors.New("borrowed functional runs are refused: the functional adapter owns and launches its harness")
		}
		resolved, err := e.resolveTarget()
		if err != nil {
			return err
		}
		if resolved.Row == nil || resolved.Row.Stale {
			return fmt.Errorf("borrowed run requires a live harness instance at %s", resolved.DataRoot)
		}
		if err := sameManagedRoot(resolved.DataRoot, plan.DataRoot); err != nil {
			return err
		}
		selectedTarget = &resolved
		e.instance = resolved.DataRoot
	}
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
	if plan.Ceiling.MaxDurationMS > 0 {
		var durationCancel context.CancelFunc
		ctx, durationCancel = context.WithTimeout(ctx, time.Duration(plan.Ceiling.MaxDurationMS)*time.Millisecond)
		defer durationCancel()
	}
	// Publish the lifecycle phase before launching a backend. Launch prints
	// and registers its bootstrap as soon as it is ready, so leaving the
	// supervisor in `created` during that window made the durable run receipt
	// contradict the instance an operator could already discover.
	if err := sup.Transition(harnessrun.StatePreparing, harnessrun.PhasePrepare); err != nil {
		return finishManagedRun(sup, ctx, err, harnessrun.FailureReadiness)
	}
	var launched *harnessclient.Launched
	if plan.Ownership == harnessrun.OwnershipFresh && plan.Adapter != harnessrun.AdapterCompare && plan.Adapter != harnessrun.AdapterFunctional {
		launched, err = launchManagedHarness(ctx, plan)
		if err != nil {
			return finishManagedRunWithCleanup(sup, ctx, err, harnessrun.FailureLauncherExit, launchedCleanup(launched))
		}
		if err := sup.RegisterProcessGroup(launchedGroup{launched: launched}); err != nil {
			return finishManagedRunWithCleanup(sup, ctx, err, harnessrun.FailureLauncherExit, launchedCleanup(launched))
		}
		if err := sup.RecordBootstrap(harnessrun.BootstrapRecord{DataRoot: launched.Bootstrap.DataRoot, DataDir: launched.Bootstrap.DataDir, URL: launched.Bootstrap.URL, PID: launched.Bootstrap.PID, Version: launched.Bootstrap.Version}); err != nil {
			return finishManagedRun(sup, ctx, err, harnessrun.FailureLauncherExit)
		}
	}
	ownerPID := os.Getpid()
	if plan.Ownership == harnessrun.OwnershipBorrowed {
		if selectedTarget != nil && selectedTarget.Row != nil {
			ownerPID = selectedTarget.Row.PID
		}
	}
	reservation, err := startRunGovernor(ctx, plan, ownerPID, func() { cancel(); _ = sup.StopProcessGroups(context.Background()) })
	if err != nil {
		return finishManagedRun(sup, ctx, err, harnessrun.FailureSafetyCeiling)
	}
	// Every adapter runs through the same lifecycle. A failure before the
	// action phase still receives a manifest and ownership disposition.
	if err := sup.Transition(harnessrun.StateReady, harnessrun.PhaseReady); err != nil {
		return finishManagedRunWithCleanup(sup, ctx, err, harnessrun.FailureReadiness, reservation.cleanup)
	}
	if err := sup.Transition(harnessrun.StateRunning, harnessrun.PhaseAction); err != nil {
		return finishManagedRunWithCleanup(sup, ctx, err, harnessrun.FailureAction, reservation.cleanup)
	}
	adapterEnv := *e
	adapterEnv.stdout = io.Discard
	adapterEnv.format = "text"
	actionErr := runPlanAdapterManaged(&adapterEnv, ctx, plan, sup)
	class := harnessrun.FailureNone
	if safetyErr := reservation.safetyError(); safetyErr != nil {
		actionErr = safetyErr
		class = harnessrun.FailureSafetyCeiling
	} else if actionErr != nil {
		class = harnessrun.FailureAction
	}
	if actionErr == nil {
		for _, artifact := range plan.Artifacts {
			if err := sup.RecordArtifact(artifact.Name); err != nil {
				actionErr = err
				class = harnessrun.FailureArtifact
				break
			}
		}
	}
	finishErr := sup.Finish(ctx, actionErr, class, reservation.cleanup)
	manifest := sup.Manifest()
	if e.jsonOutput() {
		if err := e.writeJSON(manifest); err != nil {
			return err
		}
	} else {
		e.printf("run %s %s adapter=%s state=%s\n", manifest.Plan.RunID, manifest.Plan.Workload, manifest.Plan.Adapter, manifest.State)
		if manifest.Quarantine != "" {
			e.printf("  quarantine  %s\n", manifest.Quarantine)
		}
	}
	return finishErr
}

func finishManagedRun(sup *harnessrun.Supervisor, ctx context.Context, cause error, class harnessrun.FailureClass) error {
	return finishManagedRunWithCleanup(sup, ctx, cause, class, nil)
}

func finishManagedRunWithCleanup(sup *harnessrun.Supervisor, ctx context.Context, cause error, class harnessrun.FailureClass, cleanup func(context.Context) error) error {
	if err := sup.Finish(ctx, cause, class, cleanup); err != nil {
		return err
	}
	return cause
}
