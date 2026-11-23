//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil/reporoot"
)

// TestMakeGoTestTrimsBeforeAndAfterTheTests runs `make go-test` with the
// test runner and the trim replaced by scripts that log their calls: a pass
// runs before the tests and another after them, whether they pass or fail,
// and the target keeps the tests' outcome unless a pass fails it.
func TestMakeGoTestTrimsBeforeAndAfterTheTests(t *testing.T) {
	make, err := exec.LookPath("make")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// The trim fails its second call when TRIM_AFTER_STATUS says so.
	trim := script("trim", `echo "trim $*" >> "$LOG"
[ "$(grep -c '^trim' "$LOG")" -eq 2 ] && exit "${TRIM_AFTER_STATUS:-0}"
exit 0
`)
	netns := script("netns", `echo "netns $1 $2" >> "$LOG"
exit "$NETNS_STATUS"
`)
	run := func(netnsStatus, trimAfterStatus string) (string, error) {
		t.Helper()
		if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		cmd := exec.Command(make, "--no-print-directory", "-C", reporoot.Dir(t), "go-test",
			"GO_TEST_PKGS=./cmd/ao-gocache-trim", "NETNS="+netns, "GOCACHE_TRIM="+trim)
		var env []string
		for _, kv := range os.Environ() {
			// A make running this test passes its own goals and variables.
			if !strings.HasPrefix(kv, "MAKEFLAGS=") && !strings.HasPrefix(kv, "MFLAGS=") && !strings.HasPrefix(kv, "MAKELEVEL=") {
				env = append(env, kv)
			}
		}
		cmd.Env = append(env, "CI=", "LOG="+log, "NETNS_STATUS="+netnsStatus, "TRIM_AFTER_STATUS="+trimAfterStatus)
		out, runErr := cmd.CombinedOutput()
		logged, err := os.ReadFile(log)
		if err != nil {
			t.Fatalf("make go-test ran neither: %v\n%s", err, out)
		}
		return string(logged), runErr
	}
	const want = "trim -warn\nnetns go test\ntrim -warn\n"
	for _, tc := range []struct {
		name, netnsStatus, trimAfterStatus string
		fails                              bool
	}{
		{"tests pass", "0", "0", false},
		{"tests fail", "3", "0", true},
		{"pass after fails", "0", "2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged, err := run(tc.netnsStatus, tc.trimAfterStatus)
			if logged != want {
				t.Fatalf("calls:\n%s\nwant:\n%s", logged, want)
			}
			if (err != nil) != tc.fails {
				t.Fatalf("make go-test: %v, want failure %v", err, tc.fails)
			}
		})
	}
}

// TestNoMakeTimeoutOutlastsTheTrimGuard holds longestGoCommand to the
// Makefile: every timeout a target gives a go command, a test binary's or
// golangci-lint's, plus buildAllowance for the build before it, fits in it.
// A whole-tree `make go-test` runs one binary after another under one go
// command, so its 30m is per binary; its runs take 7 to 27 minutes here.
func TestNoMakeTimeoutOutlastsTheTrimGuard(t *testing.T) {
	const buildAllowance = 30 * time.Minute
	makefile, err := os.ReadFile(reporoot.Path(t, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	timeouts := regexp.MustCompile(`(?m)^\t.*\s--?timeout[ =](\S+)`).FindAllStringSubmatch(string(makefile), -1)
	if len(timeouts) == 0 {
		t.Fatal("found no -timeout in the Makefile's recipes; the pattern no longer matches them")
	}
	for _, match := range timeouts {
		timeout, err := time.ParseDuration(match[1])
		if err != nil {
			t.Fatalf("Makefile timeout %q: %v", match[1], err)
		}
		if timeout+buildAllowance > longestGoCommand {
			t.Errorf("a Makefile target runs a go command for up to %s plus its build, past longestGoCommand (%s): raise longestGoCommand, or a trim may delete an entry that command still needs", timeout, longestGoCommand)
		}
	}
}
