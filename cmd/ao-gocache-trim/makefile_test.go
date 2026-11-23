//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
