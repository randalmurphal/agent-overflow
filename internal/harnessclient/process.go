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
	if err := proc.Kill(); err != nil && processAlive(pid) {
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
