//go:build !windows

package procutil

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// StopGroup ends a group named only by its pgid, as StopConfiguredGroup ends
// a command's: a group that ignores SIGTERM is killed once the grace has
// passed, and a group already gone reports os.ErrProcessDone.
func TestStopGroupEndsAGroupThatIgnoresSIGTERM(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 60 & echo ready; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the group never started: %q %v", line, err)
	}
	// This process is the leader's parent; reap it as init reaps an orphan.
	go func() { _ = cmd.Wait() }()
	const grace = 200 * time.Millisecond
	began := time.Now()
	if err := StopGroup(pgid, grace); err != nil {
		t.Fatalf("StopGroup = %v, want the group ended", err)
	}
	if took := time.Since(began); took < grace {
		t.Fatalf("StopGroup returned after %v, before the grace a group that ignores SIGTERM gets", took)
	}
	if err := SignalGroup(pgid, 0); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("after StopGroup the group answers %v, want gone", err)
	}
	if err := StopGroup(pgid, grace); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("StopGroup of a gone group = %v, want os.ErrProcessDone", err)
	}
}
