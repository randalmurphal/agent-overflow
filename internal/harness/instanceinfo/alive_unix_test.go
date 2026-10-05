//go:build linux || darwin

package instanceinfo

import (
	"os/exec"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
)

// TestProcessAliveCallsAnUnreapedChildDead: signal 0 still succeeds for a
// child that has exited and waits to be reaped, on Linux and macOS alike.
func TestProcessAliveCallsAnUnreapedChildDead(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for !procutil.Exited(pid) {
		if time.Now().After(deadline) {
			t.Fatal("the child never exited")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ProcessAlive(pid) {
		t.Error("ProcessAlive reports an exited, unreaped child as alive")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if procutil.Exited(pid) {
		t.Error("a reaped child still reads as exited")
	}
}
