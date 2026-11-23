//go:build !windows

package helperproc

import (
	"bufio"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// A group whose leader is gone and whose member another process reaps is
// gone once EndGroup returns, and ending it again finds nothing to end.
func TestEndGroupReturnsOnceTheGroupIsGone(t *testing.T) {
	// The leader exits at once and leaves sleep in its group, reparented.
	leader := exec.Command("/bin/sh", "-c", "sleep 60 & echo $!")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := leader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	line, readErr := bufio.NewReader(stdout).ReadString('\n')
	if err := errors.Join(readErr, leader.Wait()); err != nil {
		t.Fatalf("the leader printed %q: %v", line, err)
	}
	member, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || syscall.Kill(member, 0) != nil {
		t.Fatalf("the leader left no member running: %q %v", line, err)
	}
	EndGroup(t, pgid)
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("group %d after EndGroup: %v, want it gone", pgid, err)
	}
	EndGroup(t, pgid)
}
