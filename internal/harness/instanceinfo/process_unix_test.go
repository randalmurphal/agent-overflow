//go:build linux

package instanceinfo

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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

// parseStat takes the state and starttime from after the last ')', so a
// command name holding spaces and parentheses cannot shift the fields.
func TestParseStatReadsStateAndStartTimeAfterTheCommand(t *testing.T) {
	line := "4242 (a) b) c (d)) Z 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1234 56 18446744073709551615\n"
	state, start, err := parseStat(4242, []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if state != "Z" || start != "987654" {
		t.Fatalf("parseStat = %q, %q; want Z, 987654", state, start)
	}
	if !exitedState(state) || exitedState("S") || exitedState("R") {
		t.Fatal("exitedState does not tell a zombie from a running process")
	}
	for _, bad := range []string{"4242 (no terminator", "4242 (short) S 1 2 3"} {
		if _, _, err := parseStat(4242, []byte(bad)); err == nil {
			t.Errorf("parseStat(%q) accepted a malformed line", bad)
		}
	}
}

// ProcessStart is the reader behind ProcessIdentity.StartTime: the same
// marker for a running process, the exit of a zombie, and no process once
// it is reaped.
func TestProcessStartIsTheIdentitysBirthMarker(t *testing.T) {
	self, err := CaptureProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if start, alive, err := ProcessStart(os.Getpid()); err != nil || !alive || start != self.StartTime {
		t.Fatalf("ProcessStart(self) = %q, %v, %v; want %q, alive", start, alive, err, self.StartTime)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := CaptureProcessIdentity(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Wait()
		t.Fatal(err)
	}
	// Not reaped yet: the exited child is a zombie until Wait.
	deadline := time.Now().Add(5 * time.Second)
	for {
		start, alive, err := ProcessStart(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			if start != identity.StartTime {
				t.Fatalf("the zombie's start = %q, want %q", start, identity.StartTime)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the exited child still reads as running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Wait()
	if start, alive, err := ProcessStart(cmd.Process.Pid); err != nil || alive || start != "" {
		t.Fatalf("ProcessStart after the reap = %q, %v, %v; want no process", start, alive, err)
	}
	if _, _, err := ProcessStart(0); err == nil {
		t.Fatal("ProcessStart(0) read a process")
	}
}
