package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/harnessrun"
)

type launchedGroup struct{ launched *harnessclient.Launched }

func (g launchedGroup) Record() harnessrun.ProcessGroupRecord {
	return harnessrun.ProcessGroupRecord{ID: fmt.Sprintf("backend-%d", g.launched.PID), Owned: true, PID: g.launched.PID, GroupPID: g.launched.PID}
}

func (g launchedGroup) Terminate(ctx context.Context) error { return g.launched.Terminate(ctx) }

func (g launchedGroup) Kill(ctx context.Context) error { return g.launched.Kill(ctx) }

func launchManagedHarness(ctx context.Context, plan harnessrun.RunPlan) (*harnessclient.Launched, error) {
	binary, err := resolveBackendBinary(plan.Binary)
	if err != nil {
		return nil, err
	}
	launched, err := harnessclient.Launch(ctx, harnessclient.LaunchOptions{
		Binary: binary, DataRoot: plan.DataRoot, MockProvider: plan.MockProvider,
		Window: plan.Window, KeepHome: plan.KeepHome, DevAssetsURL: plan.DevAssetsURL,
		Timeout: 45 * time.Second, StdoutPath: filepath.Join(plan.DataRoot, "run-backend.stdout.log"),
		StderrPath: filepath.Join(plan.DataRoot, "run-backend.stderr.log"), MemoryLimitBytes: plan.Ceiling.MaxPrivateBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("launch managed harness: %w", err)
	}
	if err := launched.Bootstrap.ValidateFor(plan.DataRoot, filepath.Join(plan.DataRoot, appDataDirName)); err != nil {
		return launched, fmt.Errorf("managed harness bootstrap identity mismatch: %w", err)
	}
	return launched, nil
}

// launchedCleanup returns a supervisor callback that can still clean a
// backend when launch or manifest setup failed before its process group was
// registered. Cleanup deliberately ignores the action context: a canceled
// run must not turn a safety teardown into a live process in a quarantined
// root.
func launchedCleanup(launched *harnessclient.Launched) func(context.Context) error {
	if launched == nil {
		return nil
	}
	return func(context.Context) error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := launched.Terminate(ctx); err != nil {
			return errors.Join(err, launched.Kill(ctx))
		}
		return nil
	}
}

func sameManagedRoot(got, want string) error {
	a, err := instanceinfo.CanonicalPath(got)
	if err != nil {
		return err
	}
	b, err := instanceinfo.CanonicalPath(want)
	if err != nil {
		return err
	}
	if filepath.Clean(a) != filepath.Clean(b) {
		return fmt.Errorf("managed harness data root mismatch: got %s, want %s", got, want)
	}
	return nil
}
