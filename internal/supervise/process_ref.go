package supervise

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// ProcessRef names one process by its id and its start time, so an id the
// system reused for another process never matches. Start is the birth
// marker instanceinfo reads (instanceinfo.ProcessStart), the StartTime of
// its ProcessIdentity. It is compared, never interpreted.
type ProcessRef struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

// ErrProcessRunning is a wait that ended with the process still running.
var ErrProcessRunning = errors.New("the process is still running")

// processExitPoll is how often a wait without a process handle looks again.
const processExitPoll = 50 * time.Millisecond

// CurrentProcessRef is this process.
func CurrentProcessRef() (ProcessRef, error) { return ProcessRefOf(os.Getpid()) }

// ProcessRefOf names the process running as pid now.
func ProcessRefOf(pid int) (ProcessRef, error) {
	if pid <= 0 {
		return ProcessRef{}, fmt.Errorf("process %d is not a process id", pid)
	}
	start, alive, err := instanceinfo.ProcessStart(pid)
	if err != nil {
		return ProcessRef{}, err
	}
	if !alive {
		return ProcessRef{}, fmt.Errorf("process %d is not running", pid)
	}
	return ProcessRef{PID: pid, Start: start}, nil
}

func (r ProcessRef) valid() error {
	if r.PID <= 0 || r.Start == "" {
		return fmt.Errorf("process reference %+v lacks an id or a start time", r)
	}
	return nil
}

// Running reports whether the process r names has not exited. A process
// that is gone, has exited but not been reaped, or whose id now names a
// process that started at another time is not running.
func (r ProcessRef) Running() (bool, error) {
	if err := r.valid(); err != nil {
		return false, err
	}
	start, alive, err := instanceinfo.ProcessStart(r.PID)
	if err != nil {
		return false, err
	}
	return alive && start == r.Start, nil
}

// WaitForExit waits up to timeout for the process r names to exit. It
// returns nil once it has, ErrProcessRunning at the timeout and ctx's error
// when ctx ends first.
func WaitForExit(ctx context.Context, r ProcessRef, timeout time.Duration) error {
	if err := r.valid(); err != nil {
		return err
	}
	return waitForExit(ctx, r, timeout)
}

// pollForExit is a wait without a process handle: it looks at the process
// every processExitPoll.
func pollForExit(ctx context.Context, r ProcessRef, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		running, err := r.Running()
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("process %d did not exit within %s: %w", r.PID, timeout, ErrProcessRunning)
		}
		t := time.NewTimer(min(left, processExitPoll))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
