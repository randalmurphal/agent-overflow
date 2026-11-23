//go:build linux

package providerpath

import (
	"os/exec"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// goTestLeftover runs go test on the leftover fixture package, narrowed to
// the tests run matches, and returns its output.
func goTestLeftover(t *testing.T, run string) (string, error) {
	t.Helper()
	reporoot.Observe(t, "internal/testutil/providerpath/testdata/leftover/leftover_test.go")
	out, err := exec.Command("go", "test", "-run", run, "./testdata/leftover").CombinedOutput()
	return string(out), err
}

// A test that leaves a process running fails its package, naming it, and
// the identical run fails again: the verdict is the test binary's, so go
// test has no pass to cache.
func TestATestThatLeavesAProcessFailsItsPackageEveryRun(t *testing.T) {
	t.Parallel()
	for run := 1; run <= 2; run++ {
		out, err := goTestLeftover(t, "^TestLeavesAProcess$")
		if err == nil || strings.Contains(out, "(cached)") ||
			!strings.Contains(out, "the tests left 1 process(es) running") || !strings.Contains(out, ": sleep 30") {
			t.Fatalf("run %d: %v, want a failed run naming the sleep it left\n%s", run, err, out)
		}
	}
}

// A process on its way out when the tests end is not left running.
func TestAProcessThatExitsWithinTheGraceIsNotLeft(t *testing.T) {
	t.Parallel()
	if out, err := goTestLeftover(t, "^TestLeavesAProcessThatExitsSoon$"); err != nil {
		t.Fatalf("%v, want a pass\n%s", err, out)
	}
}
