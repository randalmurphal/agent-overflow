package main

import (
	"strings"
	"testing"
)

// TestAGoTestThatLeavesProcessesHasThemEndedAndNamed: a `go test` that
// exits leaving children that ignore SIGTERM has them killed once the
// grace has passed, before its run ends, and they are named.
func TestAGoTestThatLeavesProcessesHasThemEndedAndNamed(t *testing.T) {
	kids := t.TempDir()
	r, _, stderr, _ := fakeRunner(t, fakeChildEnv+"="+kids, childIgnoreEnv+"=1", graceEnv+"=100ms")
	inv, err := parseArgs([]string{"go", "test", "example/whole"})
	if err != nil {
		t.Fatal(err)
	}
	if status := r.run(plan(inv, nil, []string{"example/whole"}, t.TempDir()), 1); status != 0 {
		t.Fatalf("status %d; want the go test's 0", status)
	}
	checkChildrenGone(t, kids, childrenPerRun(), false)
	if out := stderr.String(); !strings.Contains(out, "ao-gotest: the go test left ") || !strings.Contains(out, "which were ended: ") {
		t.Fatalf("stderr:\n%s\nwant the processes it left named", out)
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
	if status := r.run(plan(inv, nil, []string{"example/whole"}, t.TempDir()), 1); status != 0 {
		t.Fatalf("status %d; want the go test's 0\nstderr:\n%s", status, stderr.String())
	}
}
