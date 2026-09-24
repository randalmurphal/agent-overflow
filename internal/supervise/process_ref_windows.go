//go:build windows

package supervise

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"

	"agent-overflow/internal/harness/instanceinfo"
)

// waitForExit holds one handle from the start-time check to the end of the
// wait, so the process it waits on is the one it checked.
func waitForExit(ctx context.Context, r ProcessRef, timeout time.Duration) error {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(r.PID))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("open process %d: %w", r.PID, err)
	}
	defer windows.CloseHandle(h)
	start, alive, err := instanceinfo.HandleStart(r.PID, h)
	if err != nil {
		return err
	}
	if !alive || start != r.Start {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("process %d did not exit within %s: %w", r.PID, timeout, ErrProcessRunning)
		}
		event, err := windows.WaitForSingleObject(h, uint32(min(left, processExitPoll).Milliseconds()))
		if err != nil {
			return fmt.Errorf("wait for process %d: %w", r.PID, err)
		}
		if event == windows.WAIT_OBJECT_0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
