//go:build darwin

package harnessclient

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
)

// A group whose leader has exited unreaped is gone once no member runs, and
// alive while one does. macOS answers signal 0 to the exited group with
// EPERM, so the member check is what tells them apart.
func TestOwnedGroupAliveIgnoresExitedMembers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		alive  bool
	}{
		{"every member exited", "exit 0", false},
		{"a member outlived the leader", "sleep 30 & exit 0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", tc.script)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pgid := cmd.Process.Pid
			t.Cleanup(func() {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				_ = cmd.Wait()
			})
			// The leader exits at once and stays unreaped until cleanup.
			deadline := time.Now().Add(5 * time.Second)
			for procutil.RunningInGroup(pgid, pgid) {
				if time.Now().After(deadline) {
					t.Fatalf("leader %d never exited", pgid)
				}
				time.Sleep(5 * time.Millisecond)
			}
			if got := ownedGroupAliveOS(pgid); got != tc.alive {
				t.Fatalf("ownedGroupAliveOS(%d) = %v, want %v", pgid, got, tc.alive)
			}
		})
	}
}
