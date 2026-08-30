package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

func pidFor(t target) (int, error) {
	if t.Row != nil && t.Row.PID > 0 && !t.Row.Stale {
		if err := confirmInstanceRow(t.Row.Row); err != nil {
			return 0, err
		}
		return t.Row.PID, nil
	}
	bs, err := bootstrapForTarget(t)
	if err != nil {
		return 0, fmt.Errorf("no live instance at %s: %w", t.DataRoot, err)
	}
	if !instanceinfo.ProcessAlive(bs.PID) && (bs.PIDNamespace == "" || bs.PIDNamespace == instanceinfo.CurrentPIDNamespace()) {
		return 0, fmt.Errorf("instance %s names pid %d, which is not running", t.ID, bs.PID)
	}
	return bs.PID, nil
}

func bootstrapForTarget(t target) (harnessclient.Bootstrap, error) {
	if err := validateTargetPaths(t.DataRoot, t.DataDir); err != nil {
		return harnessclient.Bootstrap{}, fmt.Errorf("target paths are unsafe: %w", err)
	}
	bs, err := harnessclient.ReadInstanceFile(t.DataDir)
	if err != nil {
		// Verbatim message, tagged: this is the one refusal --force may
		// override (see noManifestError).
		return harnessclient.Bootstrap{}, &noManifestError{msg: err.Error(), cause: err}
	}
	if err := bs.ValidateFor(t.DataRoot, t.DataDir); err != nil {
		return harnessclient.Bootstrap{}, fmt.Errorf("bootstrap identity does not match selected instance %q: %w", t.ID, err)
	}
	return bs, nil
}

func confirmInstancePID(dataDir string, pid int) error {
	bs, err := harnessclient.ReadInstanceFile(dataDir)
	if err != nil {
		return fmt.Errorf("the registry names pid %d but %s claims no instance (%v); refusing to signal a pid nothing confirms (`ao-harness list` prunes the row)", pid, dataDir, err)
	}
	if bs.PID != pid {
		return fmt.Errorf("the registry names pid %d but %s names pid %d; refusing to signal a pid the data root does not claim", pid, dataDir, bs.PID)
	}
	if err := bs.ValidateFor(filepath.Dir(dataDir), dataDir); err != nil {
		return fmt.Errorf("identity mismatch: %w", err)
	}
	if bs.PIDNamespace != "" && bs.PIDNamespace != instanceinfo.CurrentPIDNamespace() {
		return nil
	}
	if err := verifyBootstrapProcess(bs); err != nil {
		return fmt.Errorf("refusing to signal pid %d: %w", pid, err)
	}
	return nil
}

func confirmInstanceRow(row instanceinfo.Row) error {
	if err := row.Validate(); err != nil {
		return fmt.Errorf("invalid registry identity: %w", err)
	}
	if err := validateTargetPaths(row.DataRoot, row.DataDir); err != nil {
		return fmt.Errorf("unsafe target paths: %w", err)
	}
	bs, err := harnessclient.ReadInstanceFile(row.DataDir)
	if err != nil {
		return &noManifestError{msg: fmt.Sprintf("the registry names pid %d but %s claims no instance (%v); refusing to signal a pid nothing confirms (`ao-harness list` prunes the row; `ao-harness down --force` stops the pid if /proc confirms it is ours)", row.PID, row.DataDir, err), cause: err}
	}
	if err := bs.ValidateFor(row.DataRoot, row.DataDir); err != nil {
		return fmt.Errorf("identity mismatch: %w", err)
	}
	if bs.PID != row.PID {
		return fmt.Errorf("the registry names pid %d but %s names pid %d; refusing to signal a pid the data root does not claim", row.PID, row.DataDir, bs.PID)
	}
	if !row.Identity.SameLifecycle(bs.Identity) {
		return fmt.Errorf("registry/bootstrap identity mismatch for %q; refusing to signal", row.ID)
	}
	if row.Version != "" && bs.Version != "" && row.Version != bs.Version {
		return fmt.Errorf("registry/bootstrap build version mismatch (%q vs %q); refusing to signal", row.Version, bs.Version)
	}
	if bs.PIDNamespace != "" && bs.PIDNamespace != instanceinfo.CurrentPIDNamespace() {
		return nil
	}
	if err := verifyBootstrapProcess(bs); err != nil {
		return fmt.Errorf("refusing to signal pid %d: %w", row.PID, err)
	}
	return nil
}

func verifyBootstrapProcess(bs harnessclient.Bootstrap) error {
	expected := instanceinfo.ProcessIdentity{StartTime: bs.ProcessStartTime, Executable: bs.ExecutablePath, Namespace: bs.PIDNamespace}
	if expected.StartTime == "" || expected.Executable == "" || expected.Namespace == "" {
		return errors.New("process identity is incomplete; refusing destructive process fallback")
	}
	if bs.PIDNamespace != "" && bs.PIDNamespace != instanceinfo.CurrentPIDNamespace() {
		return fmt.Errorf("pid namespace %q is not this CLI's %q; refusing cross-namespace signal", bs.PIDNamespace, instanceinfo.CurrentPIDNamespace())
	}
	return instanceinfo.VerifyProcessIdentity(bs.PID, expected)
}
