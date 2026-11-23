//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/testutil/mockexec"
)

const runLauncherEnv = "AO_TEST_RUN_LAUNCHER"

// TestMain lets the test binary stand in for the launcher, so a test can
// signal a real run() from outside the way a script or terminal does.
func TestMain(m *testing.M) {
	if os.Getenv(runLauncherEnv) == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// SIGTERM to the launcher stops Playwright's whole process group, a
// descendant included, and the launcher reports the signal only after that:
// Playwright runs in a process group of its own, which no signal sent to
// the launcher reaches by itself.
func TestTheLauncherStopsTheSuiteOnSIGTERM(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "playwright.config.ts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pids := filepath.Join(dir, "suite.pids")
	// pnpm runs both the typecheck and the suite; the fake suite leaves a
	// descendant behind it and waits.
	bin := filepath.Join(dir, "bin")
	mockexec.WriteIn(t, bin, "pnpm", `#!/bin/sh
case "$*" in
  *tsc*) exit 0 ;;
esac
sleep 300 &
echo "$$ $!" > "$AO_TEST_SUITE_PIDS"
wait
`)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "--host-network", "--memory-limit-bytes", strconv.Itoa(256<<20))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		runLauncherEnv+"=1",
		"AO_TEST_SUITE_PIDS="+pids,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
	)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var suite []int
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		for _, pid := range suite {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(teardownBudget)
	for len(suite) < 2 {
		if body, err := os.ReadFile(pids); err == nil {
			suite = suite[:0]
			for _, field := range strings.Fields(string(body)) {
				if pid, err := strconv.Atoi(field); err == nil {
					suite = append(suite, pid)
				}
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("the fake suite never started:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(teardownBudget):
		t.Fatalf("the launcher did not finish after SIGTERM:\n%s", out.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("want exit 143 after SIGTERM, got %v:\n%s", err, out.String())
	}
	for _, pid := range suite {
		if err := syscall.Kill(pid, 0); err == nil {
			t.Fatalf("suite process %d outlived the launcher:\n%s", pid, out.String())
		}
	}
}
