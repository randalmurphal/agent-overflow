// Package testparallel caps how many parallel tests run at once in a test
// binary whose tests open many SQLite stores. modernc's libc takes one
// process-wide lock for every allocation, and each connection parses the
// schema, so past Cap tests at once a binary spends more CPU and finishes no
// sooner (development.md). Standard library only.
package testparallel

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Cap is the most parallel tests that run at once when -parallel is not
// given.
const Cap = 4

// reportEnv makes CapDefault print the -parallel it settled on and exit, for
// CheckApplied.
const reportEnv = "AO_TESTPARALLEL_REPORT"

// CapDefault lowers -test.parallel's default, GOMAXPROCS, to Cap. A
// -parallel given on the command line is kept, and so is a fuzzing run's,
// where it counts fuzz workers. Call it from TestMain before the tests run.
func CapDefault() error {
	flag.Parse()
	if err := capDefault(); err != nil {
		return err
	}
	if os.Getenv(reportEnv) != "" {
		fmt.Printf("parallel %s\n", flag.Lookup("test.parallel").Value)
		os.Exit(0)
	}
	return nil
}

func capDefault() error {
	given := false
	flag.Visit(func(f *flag.Flag) { given = given || f.Name == "test.parallel" })
	if given || flag.Lookup("test.fuzz").Value.String() != "" {
		return nil
	}
	parallel := flag.Lookup("test.parallel").Value.String()
	n, err := strconv.Atoi(parallel)
	if err != nil {
		return fmt.Errorf("testparallel: read -test.parallel %q: %w", parallel, err)
	}
	if n <= Cap {
		return nil
	}
	return flag.Set("test.parallel", strconv.Itoa(Cap))
}

// CheckApplied fails t unless this test binary's TestMain calls CapDefault:
// run under GOMAXPROCS 8, the binary settles on Cap without -parallel and
// keeps a -parallel of 24.
func CheckApplied(t *testing.T) {
	t.Helper()
	if got := Settled(t, "8"); got != strconv.Itoa(Cap) {
		t.Errorf("without -parallel, under GOMAXPROCS 8, this binary runs %s tests at once, want %d", got, Cap)
	}
	if got := Settled(t, "8", "-test.parallel=24"); got != "24" {
		t.Errorf("given -parallel 24, this binary runs %s tests at once", got)
	}
}

// Settled runs this test binary under gomaxprocs with args and returns the
// -parallel its TestMain settled on.
func Settled(t *testing.T, gomaxprocs string, args ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^$"}, args...)...)
	cmd.Env = append(os.Environ(), reportEnv+"=1", "GOMAXPROCS="+gomaxprocs)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run this binary under GOMAXPROCS %s %v: %v\n%s", gomaxprocs, args, err, out)
	}
	got, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "parallel ")
	if !ok {
		t.Fatalf("this binary's TestMain does not call testparallel.CapDefault: it ran its tests and printed\n%s", out)
	}
	return got
}
