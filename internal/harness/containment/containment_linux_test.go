//go:build linux

package containment

import "testing"

import (
	"os/exec"
	"strings"
)

func TestUnescapeMountField(t *testing.T) {
	if got := unescapeMountField(`/sys/fs/cgroup\040with\011tab`); got != "/sys/fs/cgroup with\ttab" {
		t.Fatalf("unescapeMountField() = %q", got)
	}
}

func TestPrepareWithFallbackInstallsInheritedDataLimit(t *testing.T) {
	group, mode, err := PrepareWithFallback(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if mode == "cgroup-v2" {
		t.Skip("host delegated cgroup v2; fallback is covered only on non-delegated hosts")
	}
	cmd := exec.Command("/bin/sh", "-c", "ulimit -d")
	if err := group.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "65536" {
		t.Fatalf("inherited data limit = %q, want 65536 KiB", out)
	}
}
