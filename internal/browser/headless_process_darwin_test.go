//go:build darwin

package browser

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"agent-overflow/internal/procutil"
)

// A child kqueue refuses is exiting. The wait reads its state until it is a
// zombie and leaves it unreaped, so its pid stays reserved for the group kill
// in stop. The refusing registration holds a running child in that state.
func TestAwaitExitLeavesARefusedChildUnreaped(t *testing.T) {
	child := exec.Command("sleep", "300")
	if err := child.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	pid := child.Process.Pid
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	done := make(chan error, 1)
	go func() { done <- awaitChildExit(pid, func(int, int) error { return unix.ESRCH }) }()
	select {
	case err := <-done:
		t.Fatalf("the wait returned (%v) while the child was running", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill the child: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("the wait did not return once the child exited")
	}
	if !procutil.Exited(pid) {
		t.Fatalf("the wait reaped child %d, giving up the pid the group kill needs", pid)
	}
}

// kqueue refuses a process from the moment it begins to exit until it is a
// zombie, which takes tens of milliseconds for a process with a large
// address space, and the member may still be closing its files meanwhile.
// The wait reads such a member's state until it is a zombie rather than
// failing or counting it as gone. The refusing registration holds a running
// member in that state for as long as the test needs.
func TestWaitGroupExitedWaitsForEveryMember(t *testing.T) {
	refuse := func(int, int) error { return unix.ESRCH }
	for _, tc := range []struct {
		name   string
		notify func(kq, pid int) error
	}{
		{name: "watched", notify: notifyOnExit},
		{name: "refused while exiting", notify: refuse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := exec.Command("sleep", "300")
			member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := member.Start(); err != nil {
				t.Fatalf("start the member: %v", err)
			}
			pgid := member.Process.Pid
			t.Cleanup(func() {
				_ = member.Process.Kill()
				_ = member.Wait()
			})

			err := waitMembersExited(pgid, 50*time.Millisecond, tc.notify)
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(pgid)) {
				t.Fatalf("the wait for a running member = %v, want an error naming %d", err, pgid)
			}

			done := make(chan error, 1)
			go func() { done <- waitMembersExited(pgid, headlessTestDeadline, tc.notify) }()
			select {
			case err := <-done:
				t.Fatalf("the wait returned (%v) while the member was running", err)
			case <-time.After(200 * time.Millisecond):
			}
			// Killed and not reaped, the member is a zombie.
			if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil {
				t.Fatalf("kill the member: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("wait: %v", err)
				}
			case <-time.After(headlessTestDeadline):
				t.Fatal("the wait did not return once the member exited")
			}
		})
	}
}
