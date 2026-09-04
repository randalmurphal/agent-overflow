//go:build darwin

package governor

import (
	"os"
	"os/exec"
	"testing"
)

// A reaped child must read as dead, not as a probe failure: current macOS
// answers the kern.proc.pid sysctl for a missing pid with zero bytes (EIO
// out of SysctlKinfoProc), and reading that as an error preserved dead
// owners' leases until TTL, blocking `ao-harness up` for a day.
func TestDarwinProcessStateReadsReapedChildAsDead(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	state, err := (darwinProcesses{}).State(pid)
	if err != nil {
		t.Fatalf("a dead pid must be an answer, not a probe error: %v", err)
	}
	if state.Alive {
		t.Fatalf("reaped child %d reads as alive", pid)
	}
}

func TestDarwinProcessStateSeesItself(t *testing.T) {
	state, err := (darwinProcesses{}).State(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Alive || state.BirthID == "" {
		t.Fatalf("own process state = %+v, want alive with a birth id", state)
	}
}

func TestDarwinProcessTreeRSSSamplesCurrentProcess(t *testing.T) {
	rss, err := (darwinProcesses{}).RSS(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if rss == 0 {
		t.Fatal("current process tree RSS is zero")
	}
}
