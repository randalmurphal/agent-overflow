package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAGoTestThatLeavesProcessesFailsAndNamesThem: a `go test` that exits
// 0 leaving children that ignore SIGTERM has them killed once the grace
// has passed, before its run ends, and the run fails, naming each child by
// its pid and, on Linux, the temp directory of the test that its command
// line names.
func TestAGoTestThatLeavesProcessesFailsAndNamesThem(t *testing.T) {
	kids := t.TempDir()
	r, _, stderr, _ := fakeRunner(t, fakeChildEnv+"="+kids, childIgnoreEnv+"=1", graceEnv+"=100ms")
	inv, err := parseArgs([]string{"go", "test", "example/whole"})
	if err != nil {
		t.Fatal(err)
	}
	if status := r.run(plan(inv, nil, []string{"example/whole"}, t.TempDir()), grant(1)); status != 1 {
		t.Fatalf("status %d; want 1 for the processes the go test left, though it exited 0\nstderr:\n%s", status, stderr.String())
	}
	checkChildrenGone(t, kids, childrenPerRun(), false)
	out := stderr.String()
	if !strings.Contains(out, "ao-gotest: the go test left ") || !strings.Contains(out, "; the run fails, since a test or fixture must end the processes it starts:\n") {
		t.Fatalf("stderr:\n%s\nwant the run failed for the processes it left", out)
	}
	for pid := range recorded(t, kids) {
		named := fmt.Sprintf("\nao-gotest:   pid %d", pid)
		if runtime.GOOS == "linux" {
			named += ", in the temp directory of a test, " + filepath.Dir(kids) + ": "
		}
		if !strings.Contains(out, named) {
			t.Errorf("stderr:\n%s\nwant child %d named as %q", out, pid, named)
		}
	}
}

// TestTestTempDirIsTheTestsDirectoryAPathNames: a leftover is placed in
// the test whose t.TempDir its command line or directory names, and in no
// other directory under the temp directory.
func TestTestTempDirIsTheTestsDirectoryAPathNames(t *testing.T) {
	t.Parallel()
	tmp := filepath.Clean(os.TempDir())
	for s, want := range map[string]string{
		"/bin/sh " + tmp + "/TestHold123/001/hold.sh":          tmp + "/TestHold123",
		"sleep 60 " + tmp + "/TestA_sub-x456/002":              tmp + "/TestA_sub-x456",
		"x " + tmp + "/BenchmarkB7/001 " + tmp + "/TestC8/001": tmp + "/BenchmarkB7",
		tmp + "/.ao-go-work/go-build1/b001/app.test":           "",
		tmp + "/TestHold123/notes":                             "",
		tmp + "/TestHold123/1000":                              tmp + "/TestHold123",
		"/elsewhere/TestHold123/001/hold.sh":                   "",
	} {
		if got := testTempDir(s); got != want {
			t.Errorf("testTempDir(%q) = %q; want %q", s, got, want)
		}
	}
}

// TestAProcessOrphanedDuringARunIsGoneOnceItExits runs a go test that
// orphans a process and waits for it to be gone. On Linux the process
// comes to the leader, the subreaper of the tree, which must wait for it
// as it exits: a zombie is still found by kill(pid, 0) and by a check that
// its group has ended, as a provider's Close makes.
func TestAProcessOrphanedDuringARunIsGoneOnceItExits(t *testing.T) {
	r, _, stderr, _ := fakeRunner(t, fakeOrphanEnv+"=1")
	inv, err := parseArgs([]string{"go", "test", "example/whole"})
	if err != nil {
		t.Fatal(err)
	}
	if status := r.run(plan(inv, nil, []string{"example/whole"}, t.TempDir()), grant(1)); status != 0 {
		t.Fatalf("status %d; want the go test's 0\nstderr:\n%s", status, stderr.String())
	}
}
