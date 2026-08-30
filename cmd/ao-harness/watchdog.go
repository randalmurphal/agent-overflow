package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

const detachedWatchdogInterval = 100 * time.Millisecond
const detachedWatchdogReadyTimeout = 5 * time.Second

type detachedWatchdogReady struct {
	Version         int    `json:"version"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
	DataRoot        string `json:"dataRoot"`
	LeaseID         string `json:"leaseId"`
	OwnerPID        int    `json:"ownerPid"`
	OwnerBirthID    string `json:"ownerBirthId"`
	WatchdogPID     int    `json:"watchdogPid"`
	WatchdogBirthID string `json:"watchdogBirthId"`
	WatchdogExe     string `json:"watchdogExecutable"`
}

func watchdogReadyPath(dataRoot string) string {
	return filepath.Join(dataRoot, appDataDirName, "logs", "harness-watchdog-ready.json")
}

func writeDetachedWatchdogReady(path string, ready detachedWatchdogReady) error {
	return atomicfile.WriteJSON(path, ready)
}

func removeWatchdogReadyFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect watchdog readiness: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("watchdog readiness path is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale watchdog readiness: %w", err)
	}
	return nil
}

func writeDetachedWatchdogState(dataRoot string, bs harnessclient.Bootstrap, limit uint64) error {
	path := filepath.Join(bs.DataDir, "logs", "harness-watchdog-state.json")
	ready, err := readDetachedWatchdogReady(watchdogReadyPath(dataRoot))
	if err != nil {
		return fmt.Errorf("read watchdog readiness for state: %w", err)
	}
	document := map[string]any{"version": 2, "pid": bs.PID, "dataRoot": dataRoot, "memoryLimitBytes": limit, "availableFloorBytes": governor.DefaultAvailableFloorBytes, "intervalMs": detachedWatchdogInterval.Milliseconds(), "leaseId": ready.LeaseID, "ownerBirthId": ready.OwnerBirthID, "watchdogPid": ready.WatchdogPID, "watchdogBirthId": ready.WatchdogBirthID, "watchdogExecutable": ready.WatchdogExe}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write watchdog state: %w", err)
	}
	return nil
}

func readDetachedWatchdogReady(path string) (detachedWatchdogReady, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return detachedWatchdogReady{}, err
	}
	var ready detachedWatchdogReady
	if err := json.Unmarshal(data, &ready); err != nil {
		return detachedWatchdogReady{}, err
	}
	if ready.Status != "armed" || ready.LeaseID == "" || ready.OwnerPID <= 0 || ready.OwnerBirthID == "" || ready.WatchdogPID <= 0 || ready.WatchdogBirthID == "" || ready.WatchdogExe == "" {
		return detachedWatchdogReady{}, errors.New("watchdog readiness is incomplete")
	}
	return ready, nil
}

type detachedWatchdogState struct {
	Version         int    `json:"version"`
	PID             int    `json:"pid"`
	DataRoot        string `json:"dataRoot"`
	LeaseID         string `json:"leaseId"`
	OwnerBirthID    string `json:"ownerBirthId"`
	WatchdogPID     int    `json:"watchdogPid"`
	WatchdogBirthID string `json:"watchdogBirthId"`
	WatchdogExe     string `json:"watchdogExecutable"`
}

type watchdogTeardownError struct {
	treeGone bool
	err      error
}

func (e *watchdogTeardownError) Error() string { return e.err.Error() }
func (e *watchdogTeardownError) Unwrap() error { return e.err }

// requireActiveDetachedWatchdog proves that the watchdog named by the state
// file is still the process that armed the exact live reservation. A stale
// state file alone is not evidence of an active memory boundary.
func requireActiveDetachedWatchdog(dataRoot string, bootstrap harnessclient.Bootstrap) error {
	path := filepath.Join(bootstrap.DataDir, "logs", "harness-watchdog-state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read watchdog state: %w", err)
	}
	var state detachedWatchdogState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse watchdog state: %w", err)
	}
	root, err := instanceinfo.CanonicalPath(dataRoot)
	if err != nil {
		return err
	}
	if state.Version != 2 || state.DataRoot != root || state.PID != bootstrap.PID || state.LeaseID == "" || state.OwnerBirthID == "" || state.WatchdogPID <= 0 || state.WatchdogBirthID == "" || state.WatchdogExe == "" {
		return errors.New("watchdog state is incomplete or does not match the selected instance")
	}
	backend, err := instanceinfo.CaptureProcessIdentity(state.PID)
	if err != nil {
		return fmt.Errorf("capture backend identity for watchdog: %w", err)
	}
	if backend.StartTime != state.OwnerBirthID || backend.StartTime != bootstrap.ProcessStartTime || backend.Executable != bootstrap.ExecutablePath {
		return errors.New("watchdog state backend identity changed")
	}
	watchdog, err := instanceinfo.CaptureProcessIdentity(state.WatchdogPID)
	if err != nil {
		return fmt.Errorf("capture watchdog identity: %w", err)
	}
	if watchdog.StartTime != state.WatchdogBirthID || watchdog.Executable != state.WatchdogExe {
		return errors.New("watchdog process identity changed")
	}
	mgr, err := governor.New(governor.Options{})
	if err != nil {
		return err
	}
	snapshot, err := mgr.Snapshot()
	if err != nil {
		return fmt.Errorf("read watchdog reservation: %w", err)
	}
	for _, lease := range snapshot.Leases {
		if lease.ID == state.LeaseID && lease.DataRoot == root && lease.OwnerPID == state.PID && lease.OwnerBirthID == state.OwnerBirthID {
			return nil
		}
	}
	return errors.New("watchdog memory reservation is not active")
}

type harnessContainmentEvidence struct {
	Version            int    `json:"version"`
	Enforcement        string `json:"enforcement"`
	WindowsJob         bool   `json:"windowsJob"`
	LinuxPID           int    `json:"linuxPid"`
	MemoryLimitBytes   uint64 `json:"memoryLimitBytes"`
	WatchdogIntervalMS int64  `json:"watchdogIntervalMs"`
	Mode               string `json:"mode"`
	DataRoot           string `json:"dataRoot"`
	LauncherPID        int    `json:"launcherPid"`
	LauncherStartTime  string `json:"launcherStartTime"`
	LauncherExecutable string `json:"launcherExecutable"`
	LauncherProfile    string `json:"launcherProfile"`
	LauncherWebview    string `json:"launcherWebviewProfile"`
}

// requireActiveHarnessBoundary is the shared attestation gate for profiling
// and other instruments that must not run without memory containment. Native
// detached instances retain the existing watchdog verification unchanged.
// Launcher-hosted WSL instances use the cross-namespace evidence record,
// because the Windows CLI cannot inspect a Linux PID through its own /proc.
func requireActiveHarnessBoundary(t target, bootstrap harnessclient.Bootstrap) error {
	if bootstrap.LauncherPIDNamespace != "windows" {
		return requireActiveDetachedWatchdog(t.DataRoot, bootstrap)
	}
	path := filepath.Join(bootstrap.DataDir, "logs", "harness-containment.json")
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("read WSL containment evidence: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("WSL containment evidence is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read WSL containment evidence: %w", err)
	}
	var evidence harnessContainmentEvidence
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return fmt.Errorf("parse WSL containment evidence: %w", err)
	}
	if evidence.Version != 1 || evidence.Enforcement != "windows-job+linux-rlimit-data" || !evidence.WindowsJob || evidence.LinuxPID <= 0 || evidence.MemoryLimitBytes != governor.DefaultCeilingBytes || evidence.WatchdogIntervalMS <= 0 {
		return errors.New("WSL containment evidence has an unsupported or incomplete schema")
	}
	if evidence.LinuxPID != bootstrap.PID || evidence.Mode != string(bootstrap.Mode) || evidence.DataRoot != bootstrap.DataRoot {
		return errors.New("WSL containment evidence does not match the selected backend")
	}
	if bootstrap.Mode != instanceinfo.ModeHarness && bootstrap.Mode != instanceinfo.ModeSoak && bootstrap.Mode != instanceinfo.ModePerf {
		return fmt.Errorf("WSL containment evidence has unsupported backend mode %q", bootstrap.Mode)
	}
	if bootstrap.LauncherPid <= 0 || bootstrap.LauncherProcessStartTime == "" || bootstrap.LauncherExecutablePath == "" || bootstrap.LauncherProfile != string(bootstrap.Mode) || bootstrap.LauncherDataRoot == "" || bootstrap.LauncherWebviewProfile == "" || bootstrap.LauncherPIDNamespace != "windows" {
		return errors.New("WSL backend launcher identity is incomplete")
	}
	if _, err := strconv.ParseInt(bootstrap.LauncherProcessStartTime, 10, 64); err != nil {
		return fmt.Errorf("WSL launcher birth identity is invalid: %w", err)
	}
	if !instanceinfo.IsAbsolutePath(bootstrap.LauncherExecutablePath) || !instanceinfo.IsAbsolutePath(bootstrap.LauncherWebviewProfile) {
		return errors.New("WSL launcher executable and WebView profile must be absolute")
	}
	if evidence.LauncherPID != bootstrap.LauncherPid || evidence.LauncherStartTime != bootstrap.LauncherProcessStartTime || evidence.LauncherExecutable != bootstrap.LauncherExecutablePath || evidence.LauncherProfile != bootstrap.LauncherProfile || evidence.LauncherWebview != bootstrap.LauncherWebviewProfile {
		return errors.New("WSL containment evidence does not match launcher identity")
	}
	if t.Row != nil && !t.Row.Identity.SameLifecycle(bootstrap.Identity) {
		return errors.New("WSL containment evidence backend identity does not match the registry")
	}
	return nil
}

func runDetachedWatchdog(args []string) (err error) {
	flags := flag.NewFlagSet("ao-harness --watchdog", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dataRoot := flags.String("data-dir", "", "harness data root")
	leaseID := flags.String("lease-id", "", "exact governor lease to monitor")
	ownerPID := flags.Int("owner-pid", 0, "backend PID")
	ownerBirthID := flags.String("owner-birth-id", "", "backend process birth identity")
	readyPath := flags.String("ready-file", "", "bounded readiness handshake file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *dataRoot == "" || *leaseID == "" || *ownerPID <= 0 || *ownerBirthID == "" || *readyPath == "" {
		return errors.New("--watchdog requires --data-dir, --lease-id, --owner-pid, --owner-birth-id, --ready-file, and no positional arguments")
	}
	ready := detachedWatchdogReady{Version: 1, DataRoot: *dataRoot, LeaseID: *leaseID, OwnerPID: *ownerPID, OwnerBirthID: *ownerBirthID}
	readyWritten := false
	defer func() {
		if readyWritten || err == nil {
			return
		}
		if writeErr := writeDetachedWatchdogReady(*readyPath, detachedWatchdogReady{Version: ready.Version, Status: "failed", Error: err.Error(), DataRoot: ready.DataRoot, LeaseID: ready.LeaseID, OwnerPID: ready.OwnerPID, OwnerBirthID: ready.OwnerBirthID}); writeErr != nil {
			err = errors.Join(err, fmt.Errorf("write watchdog failure handshake: %w", writeErr))
		}
	}()
	root, err := instanceinfo.CanonicalPath(*dataRoot)
	if err != nil {
		return err
	}
	dataDir := filepath.Join(root, appDataDirName)
	bootstrap, err := harnessclient.ReadInstanceFile(dataDir)
	if err != nil {
		return fmt.Errorf("read instance identity: %w", err)
	}
	if err := bootstrap.ValidateFor(root, dataDir); err != nil {
		return fmt.Errorf("validate instance identity: %w", err)
	}
	identity, err := instanceinfo.CaptureProcessIdentity(bootstrap.PID)
	if err != nil {
		return err
	}
	if identity.StartTime != bootstrap.ProcessStartTime || identity.Executable != bootstrap.ExecutablePath {
		return fmt.Errorf("backend process identity changed before watchdog start")
	}
	mgr, err := governor.New(governor.Options{})
	if err != nil {
		return err
	}
	lease, err := findDetachedLease(mgr, root, *ownerPID, *ownerBirthID, *leaseID)
	if err != nil {
		return err
	}
	leaseActive := true
	treeGone := false
	defer func() {
		if !leaseActive || !treeGone {
			return
		}
		if releaseErr := mgr.Release(lease); releaseErr != nil && !errors.Is(releaseErr, governor.ErrLeaseNotFound) {
			err = errors.Join(err, fmt.Errorf("release watchdog memory reservation: %w", releaseErr))
		}
	}()
	watchdogIdentity, err := instanceinfo.CaptureProcessIdentity(os.Getpid())
	if err != nil {
		return fmt.Errorf("capture watchdog process identity: %w", err)
	}
	if err := writeDetachedWatchdogReady(*readyPath, detachedWatchdogReady{Version: 1, Status: "armed", DataRoot: root, LeaseID: lease.ID, OwnerPID: lease.OwnerPID, OwnerBirthID: lease.OwnerBirthID, WatchdogPID: os.Getpid(), WatchdogBirthID: watchdogIdentity.StartTime, WatchdogExe: watchdogIdentity.Executable}); err != nil {
		return fmt.Errorf("write watchdog readiness: %w", err)
	}
	readyWritten = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan governor.Event, 1)
	monitorErr := make(chan error, 1)
	go func() {
		monitorErr <- mgr.Monitor(ctx, lease, detachedWatchdogInterval, nil, func(event governor.Event) {
			select {
			case events <- event:
			default:
			}
			cancel()
		})
	}()
	select {
	case event := <-events:
		teardownErr := handleDetachedWatchdogEvent(bootstrap, identity, event)
		if typed, ok := teardownErr.(*watchdogTeardownError); ok && typed.treeGone {
			treeGone = true
		}
		if teardownErr != nil {
			return teardownErr
		}
		treeGone = true
		return nil
	case err := <-monitorErr:
		select {
		case event := <-events:
			teardownErr := handleDetachedWatchdogEvent(bootstrap, identity, event)
			if typed, ok := teardownErr.(*watchdogTeardownError); ok && typed.treeGone {
				treeGone = true
			}
			if teardownErr != nil {
				return teardownErr
			}
			treeGone = true
			return nil
		default:
		}
		if err == nil || errors.Is(err, context.Canceled) {
			err = errors.New("watchdog monitor stopped without a safety event")
		}
		event := governor.Event{RunID: lease.RunID, Worktree: lease.Worktree, DataRoot: lease.DataRoot, Reason: governor.ReasonMonitorError, Error: err.Error(), At: time.Now().UTC()}
		teardownErr := handleDetachedWatchdogEvent(bootstrap, identity, event)
		if typed, ok := teardownErr.(*watchdogTeardownError); ok && typed.treeGone {
			treeGone = true
		}
		if teardownErr != nil {
			return teardownErr
		}
		treeGone = true
		return nil
	}
}

func findDetachedLease(mgr *governor.Manager, dataRoot string, ownerPID int, ownerBirthID, leaseID string) (governor.Lease, error) {
	snapshot, err := mgr.Snapshot()
	if err != nil {
		return governor.Lease{}, err
	}
	for _, lease := range snapshot.Leases {
		if lease.DataRoot == dataRoot && lease.ID == leaseID && lease.OwnerPID == ownerPID && lease.OwnerBirthID == ownerBirthID && len(lease.RunID) > 3 && lease.RunID[:3] == "up-" {
			return lease, nil
		}
	}
	return governor.Lease{}, fmt.Errorf("no detached harness memory reservation for %s", dataRoot)
}

func awaitDetachedWatchdogReady(cmd *os.Process, dataRoot string, lease governor.Lease, path string) error {
	deadline := time.Now().Add(detachedWatchdogReadyTimeout)
	for time.Now().Before(deadline) {
		info, err := os.Lstat(path)
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
			return errors.New("watchdog readiness path is not a regular file")
		}
		data, err := os.ReadFile(path)
		if err == nil {
			var ready detachedWatchdogReady
			if decodeErr := json.Unmarshal(data, &ready); decodeErr != nil {
				return fmt.Errorf("parse watchdog readiness: %w", decodeErr)
			}
			if ready.Status != "armed" {
				if ready.Error == "" {
					ready.Error = "watchdog reported failure without a reason"
				}
				return errors.New(ready.Error)
			}
			if ready.DataRoot != dataRoot || ready.LeaseID != lease.ID || ready.OwnerPID != lease.OwnerPID || ready.OwnerBirthID != lease.OwnerBirthID {
				return errors.New("watchdog readiness identity does not match the selected reservation")
			}
			if cmd != nil {
				if err := cmd.Release(); err != nil {
					return fmt.Errorf("release watchdog process handle: %w", err)
				}
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read watchdog readiness: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if cmd != nil {
		if killErr := cmd.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return fmt.Errorf("watchdog readiness timed out and kill failed: %w", killErr)
		}
	}
	return fmt.Errorf("watchdog did not become armed within %s", detachedWatchdogReadyTimeout)
}

func handleDetachedWatchdogEvent(bootstrap harnessclient.Bootstrap, identity instanceinfo.ProcessIdentity, event governor.Event) error {
	evidence := map[string]any{
		"version": 1, "reason": event.Reason, "pid": bootstrap.PID,
		"dataRoot": bootstrap.DataRoot, "dataDir": bootstrap.DataDir,
		"rssBytes": event.RSSBytes, "ceilingBytes": event.CeilingBytes,
		"availableBytes": event.AvailableBytes, "availableFloorBytes": event.AvailableFloorBytes,
		"at": event.At,
	}
	path := filepath.Join(bootstrap.DataDir, "logs", "harness-watchdog.json")
	data, err := json.MarshalIndent(evidence, "", "  ")
	var evidenceErr error
	if err != nil {
		evidenceErr = fmt.Errorf("encode watchdog evidence: %w", err)
	} else if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		evidenceErr = fmt.Errorf("write watchdog evidence: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var shutdownErr error
	if instanceinfo.ProcessAlive(bootstrap.PID) {
		client, err := harnessclient.Dial(ctx, bootstrap, harnessclient.Options{})
		if err != nil {
			shutdownErr = fmt.Errorf("authenticated watchdog shutdown: %w", err)
		} else {
			if _, err := client.Call(ctx, "HarnessShutdown"); err != nil {
				shutdownErr = fmt.Errorf("authenticated watchdog shutdown: %w", err)
			}
			if closeErr := client.Close(); closeErr != nil {
				shutdownErr = errors.Join(shutdownErr, fmt.Errorf("close watchdog shutdown client: %w", closeErr))
			}
		}
	}
	treeErr := harnessclient.TerminateProcessTreeVerified(ctx, bootstrap.PID, identity, 2*time.Second)
	if treeErr != nil {
		return &watchdogTeardownError{err: errors.Join(evidenceErr, shutdownErr, fmt.Errorf("reconcile watchdog process tree: %w", treeErr))}
	}
	if teardownErr := errors.Join(evidenceErr, shutdownErr); teardownErr != nil {
		return &watchdogTeardownError{treeGone: true, err: teardownErr}
	}
	return nil
}
