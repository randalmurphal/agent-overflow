package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/harness/containment"
	"agent-overflow/internal/harnessrun"
	"agent-overflow/internal/procutil"
)

func runPlanAdapter(e *env, ctx context.Context, plan harnessrun.RunPlan) error {
	return runPlanAdapterManaged(e, ctx, plan, nil)
}

type commandGroup struct{ cmd *exec.Cmd }

func (g commandGroup) Record() harnessrun.ProcessGroupRecord {
	return harnessrun.ProcessGroupRecord{ID: fmt.Sprintf("functional-%d", g.cmd.Process.Pid), Owned: true, PID: g.cmd.Process.Pid, GroupPID: g.cmd.Process.Pid}
}

func (g commandGroup) Terminate(context.Context) error {
	if g.cmd == nil || g.cmd.Process == nil {
		return nil
	}
	err := procutil.KillConfiguredGroup(g.cmd)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (g commandGroup) Kill(ctx context.Context) error { return g.Terminate(ctx) }

func runPlanAdapterManaged(e *env, ctx context.Context, plan harnessrun.RunPlan, sup *harnessrun.Supervisor) error {
	switch plan.Adapter {
	case "", harnessrun.AdapterBench:
		args := []string{plan.Workload}
		if plan.Repeat > 0 {
			args = append(args, "--repeat", strconv.Itoa(plan.Repeat))
		}
		if plan.DurationMS > 0 {
			args = append(args, "--duration", durationString(plan.DurationMS))
		}
		if plan.SampleMS > 0 {
			args = append(args, "--sample-ms", strconv.Itoa(plan.SampleMS))
		}
		if len(plan.BudgetsMS) > 0 {
			budgetValues := make([]string, 0, len(plan.BudgetsMS))
			for _, budget := range plan.BudgetsMS {
				budgetValues = append(budgetValues, strconv.FormatFloat(budget, 'g', -1, 64))
			}
			args = append(args, "--budgets", strings.Join(budgetValues, ","))
		}
		for _, meter := range plan.Meters {
			args = append(args, "--meter", meter)
		}
		for _, monitor := range plan.Monitors {
			args = append(args, "--monitor", monitor)
		}
		if plan.MonitorLeg != "" {
			args = append(args, "--monitor-leg", plan.MonitorLeg)
		}
		if plan.Leg != "" {
			args = append(args, "--leg", plan.Leg)
		}
		if plan.Trace {
			args = append(args, "--trace")
		}
		if plan.CDP != "" {
			args = append(args, "--cdp", plan.CDP)
		}
		if plan.Output != "" {
			args = append(args, "--out", plan.Output)
		}
		return runBenchDirectContext(ctx, e, args)
	case harnessrun.AdapterProfile:
		args := []string{"--thread", plan.Thread, "--scenario", plan.Scenario}
		if plan.Message != "" {
			args = append(args, plan.Message)
		}
		if plan.Output != "" {
			args = append(args, "--out", plan.Output)
		}
		if plan.IntervalUS > 0 {
			args = append(args, "--interval-us", strconv.Itoa(plan.IntervalUS))
		}
		if plan.TimeoutMS > 0 {
			args = append(args, "--timeout", durationString(plan.TimeoutMS))
		}
		if plan.CDP != "" {
			args = append(args, "--cdp", plan.CDP)
		}
		return runProfileContext(ctx, e, args)
	case harnessrun.AdapterCompare:
		return runCompareContext(ctx, e, compareAdapterArgs(plan))
	case harnessrun.AdapterFunctional:
		script, err := findFunctionalFlowScript()
		if err != nil {
			return err
		}
		flowArgs := functionalAdapterArgs(plan)
		if plan.Window {
			flowArgs = append(flowArgs, "--headed")
		}
		if plan.Binary != "" {
			flowArgs = append(flowArgs, "--binary", plan.Binary)
		}
		if plan.MockProvider != "" {
			flowArgs = append(flowArgs, "--mock-provider", plan.MockProvider)
		}
		cmd := exec.CommandContext(ctx, "node", append([]string{"--experimental-transform-types", script}, flowArgs...)...)
		procutil.ConfigureGroup(cmd)
		containmentGroup, err := containment.Prepare(plan.Ceiling.MaxPrivateBytes)
		if err != nil {
			return fmt.Errorf("install functional flow memory containment: %w", err)
		}
		defer func() { _ = containmentGroup.Close() }()
		if err := containmentGroup.Configure(cmd); err != nil {
			return fmt.Errorf("configure functional flow memory containment: %w", err)
		}
		cmd.Dir, _ = os.Getwd()
		cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start functional flow: %w", err)
		}
		if err := containmentGroup.Adopt(cmd); err != nil {
			_ = procutil.KillConfiguredGroup(cmd)
			_, _ = cmd.Process.Wait()
			return fmt.Errorf("adopt functional flow memory containment: %w", err)
		}
		if sup != nil {
			if err := sup.RegisterProcessGroup(commandGroup{cmd: cmd}); err != nil {
				killErr := procutil.KillConfiguredGroup(cmd)
				waitErr := cmd.Wait()
				return errors.Join(err, killErr, waitErr)
			}
		}
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("functional flow failed: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("run adapter %q is not supported", plan.Adapter)
	}
}

func durationString(milliseconds int64) string {
	return (time.Duration(milliseconds) * time.Millisecond).String()
}

func functionalAdapterArgs(plan harnessrun.RunPlan) []string {
	args := []string{"--spec", plan.Scenario, "--report", plan.Output, "--data-dir", plan.DataRoot, "--supervised-root"}
	if plan.MonitorLeg != "" {
		args = append(args, "--leg", plan.MonitorLeg)
	}
	if plan.PageID != "" {
		args = append(args, "--page-id", plan.PageID)
	}
	if plan.Window {
		args = append(args, "--headed")
	}
	if plan.Binary != "" {
		args = append(args, "--binary", plan.Binary)
	}
	if plan.MockProvider != "" {
		args = append(args, "--mock-provider", plan.MockProvider)
	}
	return args
}

func compareAdapterArgs(plan harnessrun.RunPlan) []string {
	args := []string{"run", "--capsule", plan.Capsule}
	// Pass every compare-owned plan value explicitly. Relying on the
	// compare command's defaults here made a durable plan silently run a
	// different measurement than the manifest described.
	args = append(args, "--window="+strconv.FormatBool(plan.Window))
	if plan.SampleMS > 0 {
		args = append(args, "--sample-ms", strconv.Itoa(plan.SampleMS))
	}
	if plan.Instrument != "" {
		args = append(args, "--instrument", plan.Instrument)
	}
	if plan.PageID != "" {
		args = append(args, "--page-id", plan.PageID)
	}
	if plan.Output != "" {
		args = append(args, "--out", plan.Output)
	}
	if plan.Pairs > 0 {
		args = append(args, "--pairs", strconv.Itoa(plan.Pairs))
	}
	if plan.BaseDir != "" {
		args = append(args, "--base-dir", plan.BaseDir)
	}
	if plan.KeepRoots {
		args = append(args, "--keep-roots")
	}
	if plan.Binary != "" {
		args = append(args, "--binary", plan.Binary)
	}
	if plan.MockProvider != "" {
		args = append(args, "--mock-provider", plan.MockProvider)
	}
	if plan.CDP != "" {
		args = append(args, "--cdp", plan.CDP)
	}
	return args
}

func findFunctionalFlowScript() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve functional flow directory: %w", err)
	}
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		path := filepath.Join(dir, "e2e", "scripts", "run-functional-flow.ts")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", fmt.Errorf("functional flow script is unavailable from %s", cwd)
}
