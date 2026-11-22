//go:build linux

package main

import (
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

// TestAKilledLauncherEndsTheSuite: SIGKILL of the launcher skips its
// teardown, and the suite runs in a process group of its own, out of reach
// of a group signal. The suite receives SIGTERM from its parent-death
// signal and stops what it started.
func TestAKilledLauncherEndsTheSuite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "playwright.config.ts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pids := filepath.Join(dir, "suite.pids")
	stopped := filepath.Join(dir, "suite.stopped")
	// The fake suite stops its descendant on SIGTERM, as Playwright stops
	// its workers.
	bin := filepath.Join(dir, "bin")
	mockexec.WriteIn(t, bin, "pnpm", `#!/bin/sh
case "$*" in
  *tsc*) exit 0 ;;
esac
sleep 300 &
child=$!
trap 'kill $child; echo stopped > "$AO_TEST_SUITE_STOPPED"; exit 143' TERM
echo "$$ $child" > "$AO_TEST_SUITE_PIDS"
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
		"AO_TEST_SUITE_STOPPED="+stopped,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
	)
	// A file rather than a pipe: Wait would wait for the suite, which
	// shares it, to close it too.
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			t.Errorf("close the output file: %v", err)
		}
	}()
	cmd.Stdout, cmd.Stderr = out, out
	output := func() string {
		data, _ := os.ReadFile(out.Name())
		return string(data)
	}
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
			t.Fatalf("the fake suite never started:\n%s", output())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for time.Now().Before(deadline.Add(teardownBudget)) {
		if _, err := os.Stat(stopped); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(stopped); err != nil {
		t.Fatalf("the suite received no SIGTERM after the launcher was killed:\n%s", output())
	}
	for _, pid := range suite {
		for !processGone(pid) {
			if time.Now().After(deadline.Add(teardownBudget)) {
				t.Fatalf("suite process %d outlived the killed launcher:\n%s", pid, output())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// processGone reports whether pid has exited: no such process, or a zombie
// that its new parent has yet to reap.
func processGone(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	text := string(data)
	fields := strings.Fields(text[strings.LastIndexByte(text, ')')+1:])
	return len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X")
}
