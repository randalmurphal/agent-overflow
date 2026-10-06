package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harness/instanceinfo"
)

// testConfigBase is the private HOME and XDG_CONFIG_HOME TestMain sets for
// the process. Written only by TestMain.
var testConfigBase string

// TestMain refuses the host-wide reservation store for the whole package. A
// test that reached it would rewrite the reservations of live instances on
// this machine; tests set env.governorDir instead. The failure is recorded as
// well as returned, so a test that tolerates the error still fails the run.
//
// It also points the OS config lookup at a private temp tree for the whole
// process, so no test can resolve the developer's real app data and
// configRootFixture needs no per-test Setenv (which would forbid t.Parallel).
func TestMain(m *testing.M) {
	configBase, err := os.MkdirTemp("", "ao-harness-test-config-")
	testConfigBase = configBase
	if err != nil {
		fmt.Fprintf(os.Stderr, "create test config root: %v\n", err)
		os.Exit(1)
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "HOME"} {
		if err := os.Setenv(key, configBase); err != nil {
			fmt.Fprintf(os.Stderr, "set %s: %v\n", key, err)
			os.Exit(1)
		}
	}
	var reached atomic.Pointer[string]
	hostGovernorDir = func() (string, error) {
		stack := string(debug.Stack())
		reached.CompareAndSwap(nil, &stack)
		return "", errors.New("test reached the host-wide harness governor directory")
	}
	code := m.Run()
	if stack := reached.Load(); stack != nil {
		fmt.Fprintf(os.Stderr, "a test reached the host-wide harness governor directory:\n%s", *stack)
		code = 1
	}
	if err := os.RemoveAll(configBase); err != nil {
		fmt.Fprintf(os.Stderr, "remove test config root: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

// plentyMemory lets a test reserve without depending on the host's free
// memory.
type plentyMemory struct{}

func (plentyMemory) AvailableMemory() (uint64, error) { return 1 << 50, nil }

// testGovernor opens the test-owned reservation store in dir.
func testGovernor(t *testing.T, dir string) *governor.Manager {
	t.Helper()
	mgr, err := governor.New(governor.Options{Dir: dir, Memory: plentyMemory{}})
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

// seedDetachedLease records the reservation `up` makes for root, owned by
// this test process so it stays live until released.
func seedDetachedLease(t *testing.T, dir, id, root string) {
	t.Helper()
	canonical, err := instanceinfo.CanonicalPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testGovernor(t, dir).Reserve(governor.Request{
		RunID: "up-" + id, Worktree: canonical, DataRoot: canonical, OwnerPID: os.Getpid(), CeilingBytes: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func assertNoLeases(t *testing.T, dir string) {
	t.Helper()
	snapshot, err := testGovernor(t, dir).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Leases) != 0 {
		t.Fatalf("reservations left in %s: %+v", dir, snapshot.Leases)
	}
}
