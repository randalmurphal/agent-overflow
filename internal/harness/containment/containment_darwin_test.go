//go:build darwin

package containment

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDarwinConfigureUsesExecWrapper(t *testing.T) {
	group, err := Prepare(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	cmd := exec.Command("/bin/echo", "hello")
	if err := group.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/bin/sh" || !strings.Contains(strings.Join(cmd.Args, " "), "exec") {
		t.Fatalf("configured command = path %q args %q", cmd.Path, cmd.Args)
	}
}
