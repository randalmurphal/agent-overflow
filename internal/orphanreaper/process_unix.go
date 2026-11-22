//go:build !windows

package orphanreaper

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Keep the sidecar outside the backend's process group. E2E and harness
// teardown signal the backend group first. If that also killed the sidecar,
// the pipe EOF reaper could never finish killing the watched groups.
func applySidecarProcessAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// writeWithoutWaiting writes as much of data to the pipe w as its buffer
// takes now and returns how much that was. os.Pipe's write end is
// non-blocking, and the write goes to the descriptor directly, so a full
// pipe ends the write instead of parking it until a reader drains the pipe.
func writeWithoutWaiting(w *os.File, data string) (int, error) {
	conn, err := w.SyscallConn()
	if err != nil {
		return 0, err
	}
	written := 0
	var writeErr error
	err = conn.Write(func(fd uintptr) bool {
		for written < len(data) {
			n, err := syscall.Write(int(fd), []byte(data[written:]))
			if n > 0 {
				written += n
			}
			switch {
			case errors.Is(err, syscall.EINTR):
			case errors.Is(err, syscall.EAGAIN):
				return true
			case err != nil:
				writeErr = err
				return true
			case n <= 0:
				return true
			}
		}
		return true
	})
	return written, errors.Join(err, writeErr)
}
