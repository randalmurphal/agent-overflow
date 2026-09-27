//go:build !windows

package app

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/threadmode"
)

// A shell that exits leaving a job in the background, as `sleep 300 &
// exit` does, ends its terminal thread at once, the way closing a terminal
// window does. The job runs on.
func TestTerminalThreadEndsWhenItsShellExitsLeavingABackgroundJob(t *testing.T) {
	app, exits := newAppWithReportedTerminalExits(t)
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	handle := openTestTerminal(t, app, "thread-term")
	pidFile := filepath.Join(t.TempDir(), "job.pid")
	script := "(trap '' HUP; exec sleep 30) & echo $! > '" + pidFile + "'; exit\n"
	if err := app.WriteTerminal(handle.TerminalID, base64.StdEncoding.EncodeToString([]byte(script))); err != nil {
		t.Fatalf("WriteTerminal: %v", err)
	}
	job := backgroundJobPID(t, pidFile)

	awaitTerminalExit(t, app, exits, handle.TerminalID)
	requireThreadRow(t, app, "thread-term", false)
	if err := syscall.Kill(job, 0); err != nil {
		t.Fatalf("the background job is gone (%v), so it did not hold the pty", err)
	}
}

// backgroundJobPID reads the pid a test shell wrote to path and kills that
// process when the test ends.
func backgroundJobPID(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		raw, err := os.ReadFile(path)
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && convErr == nil && pid > 0 {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("background job pid never written to %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
