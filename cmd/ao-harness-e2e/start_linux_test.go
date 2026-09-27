//go:build linux

package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"

	"agent-overflow/internal/harness/containment"
)

// On a host without a delegated cgroup the command starts behind the
// RLIMIT_DATA shell launcher. The recorded identity must name the command,
// not that shell.
func TestStartContainedRecordsTheCommandIdentity(t *testing.T) {
	group, enforcement, err := containment.PrepareWithFallback(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = group.Close() })
	t.Logf("containment: %s", enforcement)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(sleep)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sleep, "30")
	configureProcessGroup(cmd)
	var stderr bytes.Buffer
	identity, ok := startContained(cmd, group, &stderr)
	if !ok {
		t.Fatalf("startContained failed: %s", stderr.String())
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if identity.Executable != want {
		t.Fatalf("recorded executable = %q, want %q", identity.Executable, want)
	}
}
