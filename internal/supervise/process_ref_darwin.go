//go:build darwin

package supervise

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// processStart reads pid's start time from the kernel's process record and
// whether it has not exited. A zombie has exited.
func processStart(pid int) (start string, alive bool, err error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A missing pid answers ESRCH on some releases and, on current
		// macOS, zero bytes, which SysctlKinfoProc reports as EIO.
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EIO) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read process %d: %w", pid, err)
	}
	t := proc.Proc.P_starttime
	if t.Sec < 0 || t.Usec < 0 || t.Usec >= 1_000_000 {
		return "", false, fmt.Errorf("process %d has an invalid start time", pid)
	}
	start = strconv.FormatInt(t.Sec, 10) + "." + fmt.Sprintf("%06d", t.Usec)
	// SZOMB is 5 in <sys/proc.h>.
	return start, proc.Proc.P_stat != 5, nil
}

func waitForExit(ctx context.Context, r ProcessRef, timeout time.Duration) error {
	return pollForExit(ctx, r, timeout)
}
