package harnessclient

import (
	"context"
	"fmt"
	"os"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

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
		if !instanceinfo.ProcessAlive(pid) {
			// It died between the liveness check the caller made and this
			// signal. Nothing to do, and nothing went wrong.
			return nil
		}
		return fmt.Errorf("terminate %d: %w", pid, err)
	}

	const pollInterval = 50 * time.Millisecond
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !instanceinfo.ProcessAlive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if err := proc.Kill(); err != nil && instanceinfo.ProcessAlive(pid) {
		return fmt.Errorf("kill %d after %s grace: %w", pid, grace, err)
	}
	return nil
}
