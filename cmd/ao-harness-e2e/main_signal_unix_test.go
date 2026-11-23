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

// launcherFixture lays out a run the launcher accepts: what harness-build
// leaves, a git checkout for the typecheck record, a suite directory, an
// installed browser, and pnpm as a script on PATH that answers Playwright's
// install probe and the typecheck and otherwise runs suite as the suite.
// It returns the suite directory and the environment for the launcher.
func launcherFixture(t *testing.T, suite string) (string, []string) {
	t.Helper()
	in, err := parseRunInputs(runInputsMk)
	if err != nil {
		t.Fatal(err)
	}
	root := harnessRoot(t, in)
	gitEnv := []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
	git := exec.Command("git", "init", "-q", root)
	git.Env = append(os.Environ(), gitEnv...)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	testDir := filepath.Join(root, "e2e")
	writeFile(t, filepath.Join(testDir, "playwright.config.ts"), "", 0o600)
	browser := filepath.Join(t.TempDir(), "chromium-1")
	writeFile(t, filepath.Join(browser, "INSTALLATION_COMPLETE"), "", 0o644)
	bin := t.TempDir()
	mockexec.WriteIn(t, bin, "pnpm", `#!/bin/sh
case "$*" in
  *"playwright install --dry-run"*) printf 'Chromium 1\n  Install location:    %s\n' "$AO_TEST_BROWSER"; exit 0 ;;
  *tsc*) exit 0 ;;
esac
`+suite)
	return testDir, append(gitEnv,
		runLauncherEnv+"=1",
		"AO_TEST_BROWSER="+browser,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CACHE_HOME="+filepath.Join(t.TempDir(), "cache"),
	)
}

// SIGTERM to the launcher stops Playwright's whole process group, a
// descendant included, and the launcher reports the signal only after that:
// Playwright runs in a process group of its own, which no signal sent to
// the launcher reaches by itself.
func TestTheLauncherStopsTheSuiteOnSIGTERM(t *testing.T) {
	// The fake suite leaves a descendant behind it and waits.
	pids := filepath.Join(t.TempDir(), "suite.pids")
	dir, env := launcherFixture(t, `sleep 300 &
echo "$$ $!" > "$AO_TEST_SUITE_PIDS"
wait
`)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "--host-network", "--memory-limit-bytes", strconv.Itoa(256<<20))
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), env...), "AO_TEST_SUITE_PIDS="+pids)
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

// A run missing a declared input fails before the typecheck and the suite,
// naming the input.
func TestTheLauncherRefusesARunMissingAnInput(t *testing.T) {
	started := filepath.Join(t.TempDir(), "suite.started")
	dir, env := launcherFixture(t, `echo started > "$AO_TEST_SUITE_STARTED"
`)
	if err := os.Remove(filepath.Join(filepath.Dir(dir), "bin", "ao-mockforge")); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "--host-network", "--memory-limit-bytes", strconv.Itoa(256<<20))
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), env...), "AO_TEST_SUITE_STARTED="+started)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(out), "bin/ao-mockforge: not an executable file") {
		t.Fatalf("launcher without bin/ao-mockforge = %v:\n%s", err, out)
	}
	if strings.Contains(string(out), "TypeScript") {
		t.Fatalf("the launcher typechecked before its preflight:\n%s", out)
	}
	if _, err := os.Stat(started); err == nil {
		t.Fatalf("the suite started without bin/ao-mockforge:\n%s", out)
	}
}
