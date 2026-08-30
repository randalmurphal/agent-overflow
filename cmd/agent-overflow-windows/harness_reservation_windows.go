//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/wsllauncher"
)

const (
	launcherReservationTTL      = 2 * time.Minute
	launcherReservationInterval = 100 * time.Millisecond
)

// acquireHarnessReservation claims the combined native-launcher and WSL
// budget in the host-global governor. The Windows Job Object and the WSL
// watchdog enforce the two namespace-local sides. The reservation is one
// claim, so concurrent worktrees cannot each assume they own the full budget.
func (a *launcherApp) acquireHarnessReservation(distro string) error {
	if activeProfile == "" {
		return nil
	}
	a.mu.Lock()
	if a.memoryLease != nil {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	manager, err := governor.New(governor.Options{LeaseTTL: launcherReservationTTL})
	if err != nil {
		return fmt.Errorf("create launcher memory governor: %w", err)
	}
	identity, err := instanceinfo.CaptureProcessIdentity(os.Getpid())
	if err != nil {
		return fmt.Errorf("capture launcher reservation identity: %w", err)
	}
	worktree, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("capture launcher worktree: %w", err)
	}
	worktree, err = instanceinfo.CanonicalPath(worktree)
	if err != nil {
		return fmt.Errorf("canonicalize launcher worktree: %w", err)
	}
	// The launcher profile intentionally owns one WSL data root per machine.
	// Use a stable synthetic root for the reservation rather than a Windows
	// spelling of the Linux data root, which is unavailable until WSL starts.
	// Hashing the distro keeps arbitrary distro names out of path components.
	distroHash := sha256.Sum256([]byte(distro))
	root := filepath.Join(os.TempDir(), "agent-overflow-wsl-reservation", activeProfile, hex.EncodeToString(distroHash[:8]))
	lease, err := manager.Reserve(governor.Request{
		RunID:        "wsl-" + activeProfile + "-" + hex.EncodeToString(distroHash[:8]),
		Worktree:     worktree,
		DataRoot:     root,
		OwnerPID:     os.Getpid(),
		OwnerBirthID: identity.StartTime,
		CeilingBytes: governor.DefaultCeilingBytes,
		TTL:          launcherReservationTTL,
	})
	if err != nil {
		return fmt.Errorf("reserve combined Windows/WSL harness memory: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	if a.memoryLease != nil {
		a.mu.Unlock()
		cancel()
		_ = manager.Release(lease)
		return nil
	}
	a.memoryGovernor = manager
	a.memoryLease = &lease
	a.memoryLeaseCancel = cancel
	a.mu.Unlock()

	go a.monitorHarnessReservation(ctx, manager, lease)
	return nil
}

func (a *launcherApp) monitorHarnessReservation(ctx context.Context, manager *governor.Manager, lease governor.Lease) {
	errCh := make(chan error, 1)
	go func() {
		errCh <- manager.Monitor(ctx, lease, launcherReservationInterval, nil, func(event governor.Event) {
			log.Printf("harness memory governor: profile=%s reason=%s available=%d floor=%d rss=%d ceiling=%d", activeProfile, event.Reason, event.AvailableBytes, event.AvailableFloorBytes, event.RSSBytes, event.CeilingBytes)
			a.stopForMemorySafety("host memory reservation crossed " + event.Reason)
		})
	}()
	ticker := time.NewTicker(launcherReservationTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errCh:
			if err != nil && !strings.Contains(err.Error(), "no longer live") {
				log.Printf("harness memory governor: monitor failed: %v", err)
				a.stopForMemorySafety("host memory monitor failed")
			}
			return
		case <-ticker.C:
			renewed, err := manager.Renew(lease)
			if err != nil {
				log.Printf("harness memory governor: renew failed: %v", err)
				a.stopForMemorySafety("host memory reservation renew failed")
				return
			}
			lease = renewed
		}
	}
}

func (a *launcherApp) stopForMemorySafety(reason string) {
	log.Printf("harness memory governor: stopping isolated launcher: %s", reason)
	a.mu.Lock()
	launcher := a.launcher
	app := a.wails
	a.mu.Unlock()
	if launcher != nil {
		if err := launcher.Stop(); err != nil {
			log.Printf("harness memory governor: stop WSL launcher: %v", err)
		}
	}
	if app != nil {
		app.Quit()
	}
}

// releaseHarnessReservation is called only after the WSL backend and its
// Windows wrapper have both been confirmed stopped. If either side remains
// uncertain, the lease stays visible and the dead-owner pruning rules handle
// a process crash without freeing capacity early.
func (a *launcherApp) releaseHarnessReservation() error {
	a.mu.Lock()
	manager := a.memoryGovernor
	lease := a.memoryLease
	cancel := a.memoryLeaseCancel
	a.memoryGovernor = nil
	a.memoryLease = nil
	a.memoryLeaseCancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if manager == nil || lease == nil {
		return nil
	}
	if err := manager.Release(*lease); err != nil {
		return fmt.Errorf("release combined Windows/WSL harness memory: %w", err)
	}
	return nil
}

func writeWSLContainmentEvidence(ctx context.Context, distro, binPath string, bs *wsllauncher.Bootstrap) error {
	if activeProfile == "" || bs == nil || bs.PID <= 0 {
		return fmt.Errorf("WSL containment evidence needs an isolated backend pid")
	}
	home, ok := wslHomeFromBinary(binPath)
	if !ok {
		return fmt.Errorf("derive WSL home from backend path %q", binPath)
	}
	rootName := map[string]string{
		"harness": ".agent-overflow-harness",
		"soak":    ".agent-overflow-soak",
		"perf":    ".agent-overflow-perf",
	}[activeProfile]
	if rootName == "" {
		return fmt.Errorf("unknown isolated launcher profile %q", activeProfile)
	}
	dataRoot := filepath.ToSlash(home + "/" + rootName)
	launcher, err := instanceinfo.CaptureProcessIdentity(os.Getpid())
	if err != nil {
		return fmt.Errorf("capture launcher identity for containment evidence: %w", err)
	}
	mode := launcherRuntimeMode()
	evidenceDocument := struct {
		Version             int    `json:"version"`
		Enforcement         string `json:"enforcement"`
		WindowsJob          bool   `json:"windowsJob"`
		LinuxPID            int    `json:"linuxPid"`
		MemoryLimitBytes    uint64 `json:"memoryLimitBytes"`
		WatchdogIntervalMS  int64  `json:"watchdogIntervalMs"`
		Mode                string `json:"mode"`
		DataRoot            string `json:"dataRoot"`
		LauncherPID         int    `json:"launcherPid"`
		LauncherStartTime   string `json:"launcherStartTime"`
		LauncherExecutable  string `json:"launcherExecutable"`
		LauncherProfile     string `json:"launcherProfile"`
		LauncherWebviewPath string `json:"launcherWebviewProfile"`
	}{
		Version:             1,
		Enforcement:         "windows-job+linux-rlimit-data",
		WindowsJob:          true,
		LinuxPID:            bs.PID,
		MemoryLimitBytes:    governor.DefaultCeilingBytes,
		WatchdogIntervalMS:  wslMemoryWatchInterval.Milliseconds(),
		Mode:                activeProfile,
		DataRoot:            dataRoot,
		LauncherPID:         os.Getpid(),
		LauncherStartTime:   launcher.StartTime,
		LauncherExecutable:  launcher.Executable,
		LauncherProfile:     activeProfile,
		LauncherWebviewPath: webviewDataDir(mode),
	}
	evidenceBytes, err := json.Marshal(evidenceDocument)
	if err != nil {
		return fmt.Errorf("marshal WSL containment evidence: %w", err)
	}
	evidence := string(evidenceBytes)
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "--", "/bin/sh", "-c", wslContainmentEvidenceScript, "agent-overflow-containment-evidence", dataRoot, evidence)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write WSL containment evidence: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func wslHomeFromBinary(binPath string) (string, bool) {
	const suffix = "/.local/bin/agent-overflow"
	if !strings.HasSuffix(binPath, suffix) {
		return "", false
	}
	home := strings.TrimSuffix(binPath, suffix)
	return home, home != "" && home != "/"
}

const wslContainmentEvidenceScript = `
set -eu
root="$1"
document="$2"
logdir="$root/agent-overflow/logs"
mkdir -p "$logdir"
tmp="$logdir/harness-containment.json.tmp.$$"
trap 'rm -f "$tmp"' EXIT
printf '%s' "$document" > "$tmp"
chmod 600 "$tmp"
mv -f "$tmp" "$logdir/harness-containment.json"
[ "$(cat "$logdir/harness-containment.json")" = "$document" ]
trap - EXIT
`
