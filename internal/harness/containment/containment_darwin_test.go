//go:build darwin

package containment

import (
	"os/exec"
	"testing"
)

func TestDarwinConfigurePreservesCommandForWatchdogEnforcement(t *testing.T) {
	group, err := Prepare(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	cmd := exec.Command("/bin/echo", "hello")
	wantPath := cmd.Path
	wantArgs := append([]string(nil), cmd.Args...)
	if err := group.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Path != wantPath || len(cmd.Args) != len(wantArgs) || cmd.Args[0] != wantArgs[0] || cmd.Args[1] != wantArgs[1] {
		t.Fatalf("configured command = path %q args %q", cmd.Path, cmd.Args)
	}
}

func TestDarwinPrepareReportsWatchdogOnlyEnforcement(t *testing.T) {
	group, mode, err := PrepareWithFallback(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if mode != "watchdog-only-darwin" {
		t.Fatalf("enforcement mode = %q", mode)
	}
}
