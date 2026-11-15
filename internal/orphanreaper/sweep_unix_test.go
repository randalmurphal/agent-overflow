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

// The sweep names a leader by pid and start time as the production process
// table reports them. A live leader recorded under a parent it no longer
// has is reaped when its start time matches, and left running when the
// recorded start time is another process's: the pid was reused.
func TestSweepReapsTheRecordedLeaderAndNotAProcessThatReusedItsPid(t *testing.T) {
	for _, c := range []struct {
		name   string
		shift  int64
		reaped bool
	}{
		{"the recorded leader", 0, true},
		{"a process that reused the pid", 10_000, false},
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
			created := CaptureCreateUnix(pid)
			if created == 0 {
				t.Fatal("the process table reported no start time")
			}
			registry := NewRegistry(filepath.Join(t.TempDir(), "orphan-registry.json"))
			// Recorded by a parent other than this process, as a leader whose
			// backend died is.
			if err := registry.Add(Record{
				Owner: "test", PID: pid, PGID: pid, CreateUnix: created + c.shift, ParentPID: os.Getppid(),
			}); err != nil {
				t.Fatal(err)
			}
			if err := Sweep(registry, GopsutilProcInfo(), 50*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			select {
			case <-exited:
				if !c.reaped {
					t.Fatal("the sweep killed a process whose start time is not the recorded one")
				}
			case <-time.After(2 * time.Second):
				if c.reaped {
					t.Fatal("the sweep left the recorded leader running")
				}
			}
		})
	}
}
