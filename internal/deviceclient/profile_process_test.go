package deviceclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProfileProcessLockHelper(t *testing.T) {
	dir := os.Getenv("AO_TEST_PROFILE_LOCK_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	release, err := lockProfile(t.Context(), dir, "profile")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fmt.Fprintln(os.Stdout, "profile-locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestProfileLockSurvivesContentionAndReleasesAfterProcessCrash(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestProfileProcessLockHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "AO_TEST_PROFILE_LOCK_DIR="+dir)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(output)
	ready := false
	for scanner.Scan() {
		if scanner.Text() == "profile-locked" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("child did not acquire the profile lock")
	}
	attempt, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	unlock, err := lockProfile(attempt, dir, "profile")
	stop()
	if unlock != nil {
		unlock()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process contention: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true
	unlock, err = lockProfile(ctx, dir, "profile")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
