//go:build !windows

package orphanreaper

import (
	"errors"
	"os/exec"
	"testing"
)

// An identity read twice names the same process, and a pid that no longer
// names a process reads as gone.
func TestIdentityNamesOneProcessUntilItIsGone(t *testing.T) {
	start := func() *exec.Cmd {
		cmd := exec.Command("sleep", "600")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		return cmd
	}
	first, second := start(), start()
	a, err := Identify(first.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Identify(first.Process.Pid)
	if err != nil || again != a {
		t.Fatalf("a second read of the same process = %+v %v, want %+v", again, err, a)
	}
	if a.Start == "" {
		t.Fatalf("identity %+v carries no start", a)
	}
	b, err := Identify(second.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// Two processes started within one clock tick share a start: an
	// identity names a process only together with its pid.
	if b.Boot != a.Boot {
		t.Fatalf("two processes of one boot read boots %q and %q", a.Boot, b.Boot)
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	if id, err := Identify(first.Process.Pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("a reaped process reads %+v %v, want ErrNoProcess", id, err)
	}
}
