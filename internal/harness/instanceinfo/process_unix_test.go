//go:build linux

package instanceinfo

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A binary replaced on disk after spawn (the harness-rebuild case) makes
// /proc/<pid>/exe read "<path> (deleted)". The captured identity must
// still match the record taken at spawn, or `ao-harness down` refuses
// the process it started.
func TestCaptureProcessIdentityStripsDeletedMarker(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	src, err := os.Open(sleepPath)
	if err != nil {
		t.Fatalf("open sleep: %v", err)
	}
	defer src.Close()
	copied := filepath.Join(t.TempDir(), "sleep-copy")
	dst, err := os.OpenFile(copied, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatalf("create copy: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatalf("copy sleep: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close copy: %v", err)
	}

	cmd := exec.Command(copied, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start copy: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	before, err := CaptureProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("capture before delete: %v", err)
	}
	if err := os.Remove(copied); err != nil {
		t.Fatalf("remove running binary: %v", err)
	}
	after, err := CaptureProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("capture after delete: %v", err)
	}
	if after.Executable != before.Executable {
		t.Fatalf("executable changed across on-disk delete: before %q, after %q", before.Executable, after.Executable)
	}
	if err := VerifyProcessIdentity(cmd.Process.Pid, before); err != nil {
		t.Fatalf("verify against spawn-time record after delete: %v", err)
	}
}
