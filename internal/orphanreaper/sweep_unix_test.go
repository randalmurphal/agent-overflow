//go:build !windows

package orphanreaper

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The sweep names a leader by pid and identity as the production process
// table reports them. A live leader recorded under a parent it no longer
// has is reaped when its identity matches, and left running when the
// recorded identity is another process's: the pid was reused.
func TestSweepReapsTheRecordedLeaderAndNotAProcessThatReusedItsPid(t *testing.T) {
	for _, c := range []struct {
		name   string
		other  bool
		reaped bool
	}{
		{"the recorded leader", false, true},
		{"a process that reused the pid", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			victim := exec.Command("sleep", "120")
			victim.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := victim.Start(); err != nil {
				t.Fatal(err)
			}
			pid := victim.Process.Pid
			t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
			exited := make(chan struct{})
			go func() {
				_, _ = victim.Process.Wait()
				close(exited)
			}()
			// Recorded by a parent other than this process, as a leader whose
			// backend died is.
			leader, err := capture(SystemProcInfo(), pid, os.Getppid())
			if err != nil {
				t.Fatal(err)
			}
			if c.other {
				leader.identity.Start += "0"
			}
			registry := NewRegistry(filepath.Join(t.TempDir(), "orphan-registry.json"))
			if err := registry.Add("test", leader); err != nil {
				t.Fatal(err)
			}
			if err := Sweep(registry, SystemProcInfo(), 50*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			if recs, err := registry.Load(); err != nil || len(recs) != 0 {
				t.Fatalf("the sweep left %+v %v in the registry", recs, err)
			}
			select {
			case <-exited:
				if !c.reaped {
					t.Fatal("the sweep killed a process whose identity is not the recorded one")
				}
			case <-time.After(2 * time.Second):
				if c.reaped {
					t.Fatal("the sweep left the recorded leader running")
				}
			}
		})
	}
}
