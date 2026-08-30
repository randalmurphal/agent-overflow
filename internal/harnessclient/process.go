package harnessclient

import (
	"context"
	"fmt"
	"os"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// pollInterval is how often liveness is re-probed while waiting for a
// signal to take effect.
const pollInterval = 50 * time.Millisecond

// KillConfirmWindow is how long TerminateProcess waits for a delivered
// SIGKILL to actually remove the process before reporting that it
// survived. A package var so a test does not have to spend it.
var KillConfirmWindow = 2 * time.Second

// processAlive is the liveness probe, indirected so a test can act out
// the case this function exists to report — a process that outlives
// SIGKILL — which no cooperating child can be made to do on demand.
var processAlive = instanceinfo.ProcessAlive

// verifyProcessIdentity is indirected so lifecycle tests can exercise a PID
// recycling race without depending on the host scheduler to recycle a real
// process ID during the grace window.
var verifyProcessIdentity = instanceinfo.VerifyProcessIdentity

// verifyProcessIfAlive closes the normal-exit race between a liveness probe
// and identity capture. A process that disappears during verification is
// already safe. A process that remains, including a recycled PID, must still
// match the complete expected identity before a caller may signal it.
func verifyProcessIfAlive(pid int, expected instanceinfo.ProcessIdentity) (bool, error) {
	if !processAlive(pid) {
		return false, nil
	}
	if err := verifyProcessIdentity(pid, expected); err != nil {
		if !processAlive(pid) {
			return false, nil
		}
		return true, err
	}
	return true, nil
}

// WaitForExit confirms a process actually disappeared after an authenticated
// shutdown request. A successful RPC only proves the intended backend heard
// the request, not that its cleanup has finished.
func WaitForExit(ctx context.Context, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("wait for exit: %d is not a pid", pid)
	}
	for processAlive(pid) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for pid %d to exit: %w", pid, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
	return nil
}

// TerminateProcess asks a backend to stop the way a Ctrl-C does, and
// escalates to a kill if it has not gone within grace.
//
// The polite signal matters beyond politeness: the graceful shutdown
// path is what withdraws the instance's registry row and data-dir
// instance file. A killed instance leaves both behind, which a reader
// then has to recognize as stale — correct, but noisier.
func TerminateProcess(ctx context.Context, pid int, grace time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("terminate: %d is not a pid", pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("terminate %d: %w", pid, err)
	}
	if err := requestStop(proc); err != nil {
		if !processAlive(pid) {
			// It died between the liveness check the caller made and this
			// signal. Nothing to do, and nothing went wrong.
			return nil
		}
		return fmt.Errorf("terminate %d: %w", pid, err)
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if err := requestKill(proc); err != nil && processAlive(pid) {
		return fmt.Errorf("kill %d after %s grace: %w", pid, grace, err)
	}
	// A successful Kill() is a signal DELIVERED, not a process gone: an
	// uninterruptible sleep (a wedged WSL9p read, an NFS stall) outlives
	// SIGKILL, and a caller that printed "stopped" over a still-running
	// backend is how two of them end up on one SQLite file. Confirm.
	killDeadline := time.Now().Add(KillConfirmWindow)
	for {
		if !processAlive(pid) {
			return nil
		}
		if !time.Now().Before(killDeadline) {
			return fmt.Errorf("pid %d survived SIGKILL after %s", pid, KillConfirmWindow)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// TerminateProcessVerified is the destructive fallback for a backend that
// does not expose authenticated shutdown. It rechecks process birth,
// executable, and namespace immediately before signalling and again before
// escalation. Callers should prefer ShutdownAuthenticated.
func TerminateProcessVerified(ctx context.Context, pid int, expected instanceinfo.ProcessIdentity, grace time.Duration) error {
	if expected.StartTime == "" || expected.Executable == "" || expected.Namespace == "" {
		return fmt.Errorf("terminate %d: incomplete process identity evidence", pid)
	}
	if expected.Namespace != "" && expected.Namespace != instanceinfo.CurrentPIDNamespace() {
		return fmt.Errorf("terminate %d: pid namespace %q is not this CLI's %q", pid, expected.Namespace, instanceinfo.CurrentPIDNamespace())
	}
	alive, err := verifyProcessIfAlive(pid, expected)
	if err != nil {
		return fmt.Errorf("terminate %d: process identity changed: %w", pid, err)
	}
	if !alive {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("terminate %d: %w", pid, err)
	}
	alive, err = verifyProcessIfAlive(pid, expected)
	if err != nil {
		return fmt.Errorf("terminate %d: process identity changed before signal: %w", pid, err)
	}
	if !alive {
		return nil
	}
	if err := requestStop(proc); err != nil {
		if !processAlive(pid) {
			return nil
		}
		return fmt.Errorf("terminate %d: %w", pid, err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if !processAlive(pid) {
		return nil
	}
	// Do not escalate against a recycled pid. This second birth check is
	// the point of this fallback over TerminateProcess.
	alive, err = verifyProcessIfAlive(pid, expected)
	if err != nil {
		return fmt.Errorf("terminate %d: process identity changed before kill: %w", pid, err)
	}
	if !alive {
		return nil
	}
	if err := requestKill(proc); err != nil && processAlive(pid) {
		return fmt.Errorf("kill %d after %s grace: %w", pid, grace, err)
	}
	killDeadline := time.Now().Add(KillConfirmWindow)
	for {
		if !processAlive(pid) {
			return nil
		}
		if !time.Now().Before(killDeadline) {
			return fmt.Errorf("pid %d survived SIGKILL after %s", pid, KillConfirmWindow)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// TerminateProcessTreeVerified reconciles the authenticated process and every
// descendant that belongs to its launch boundary. It returns only after the
// owned tree is gone, including the case where the backend leader already
// exited and left a child behind.
func TerminateProcessTreeVerified(ctx context.Context, pid int, expected instanceinfo.ProcessIdentity, grace time.Duration) error {
	if expected.StartTime == "" || expected.Executable == "" || expected.Namespace == "" {
		return fmt.Errorf("terminate tree %d: incomplete process identity evidence", pid)
	}
	if expected.Namespace != instanceinfo.CurrentPIDNamespace() {
		return fmt.Errorf("terminate tree %d: pid namespace %q is not this CLI's %q", pid, expected.Namespace, instanceinfo.CurrentPIDNamespace())
	}
	return terminateOwnedProcess(ctx, pid, expected, grace)
}
