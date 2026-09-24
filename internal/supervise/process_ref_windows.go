//go:build windows

package supervise

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

// processStart reads pid's creation time through a handle and whether it
// has not exited.
func processStart(pid int) (start string, alive bool, err error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	return handleStart(pid, h)
}

func handleStart(pid int, h windows.Handle) (start string, alive bool, err error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", false, fmt.Errorf("read process %d times: %w", pid, err)
	}
	return strconv.FormatInt(created.Nanoseconds(), 10), exited.HighDateTime == 0 && exited.LowDateTime == 0, nil
}

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
	start, alive, err := handleStart(r.PID, h)
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
