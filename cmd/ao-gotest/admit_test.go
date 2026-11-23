package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/harness/governor"
)

// TestBucketTotalsCapAtACorePerBucketAndOnePackagesBuckets: a run
// reserves for at most a bucket per core and at most one package's
// buckets, the largest first, and k buckets at once hold the k largest
// units together.
func TestBucketTotalsCapAtACorePerBucketAndOnePackagesBuckets(t *testing.T) {
	t.Parallel()
	mib := func(units ...uint64) []uint64 {
		for k := range units {
			units[k] <<= 20
		}
		return units
	}
	appStore := append(mib(slices.Repeat([]uint64{640}, 8)...), mib(slices.Repeat([]uint64{1664}, 8)...)...)
	for _, tc := range []struct {
		units []uint64
		cores int
		want  []uint64
	}{
		{appStore, 24, mib(1664, 3328, 4992, 6656, 8320, 9984, 11648, 13312)},
		{appStore, 4, mib(1664, 3328, 4992, 6656)},
		{appStore, 1, mib(1664)},
		{appStore, 0, mib(1664)},
		{mib(slices.Repeat([]uint64{640}, 8)...), 24, mib(640, 1280, 1920, 2560, 3200, 3840, 4480, 5120)},
		{mib(640, 1664, 640), 8, mib(1664, 2304, 2944)},
	} {
		if got := bucketTotals(tc.units, tc.cores); !slices.Equal(got, tc.want) {
			t.Errorf("%v on %d cores: totals %v; want %v", tc.units, tc.cores, got, tc.want)
		}
	}
}

// TestEachPhaseHoldsOnlyItsOwnLease runs a split package whose list step
// is declared at 7168 MiB and its buckets at 1664 MiB, one bucket at a
// time, with 7168 MiB free above the governor's floor. The list step runs
// under a lease of 7168 MiB, released before the buckets ask for theirs;
// while the bucket runs, the run holds 1664 MiB, and a second run can
// reserve the 5504 MiB between them.
func TestEachPhaseHoldsOnlyItsOwnLease(t *testing.T) {
	const mib, floor = 1 << 20, 1 << 30
	r, _, stderr, log := fakeRunner(t, fakeHoldEnv+"=1")
	r.cores = 1
	m := testGovernor(t, floor+7168*mib)
	inner := r.admissionIn(m, governor.Request{RunID: "go-test", Worktree: t.TempDir(), DataRoot: t.TempDir(), OwnerPID: os.Getpid()})
	var mu sync.Mutex
	var leases []string
	admit := func(p phase, totals []uint64) (int, func() error, error) {
		w, release, err := inner(p, totals)
		if err != nil {
			return w, release, err
		}
		mu.Lock()
		leases = append(leases, fmt.Sprintf("%s: %d MiB", p.jobs, totals[w-1]>>20))
		mu.Unlock()
		return w, func() error {
			mu.Lock()
			leases = append(leases, "released "+p.jobs)
			mu.Unlock()
			return release()
		}, nil
	}
	inv, err := parseArgs([]string{"go", "test", "-count=1", "example/split"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- r.run(plan(inv, []split{{"example/split", 1664 * mib, 7168 * mib}}, nil, t.TempDir()), admit)
	}()
	deadline := time.Now().Add(time.Minute)
	for strings.Count(readFile(t, log), "held ") < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("no bucket held:\n%s\n%s", readFile(t, log), stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap, err := m.Snapshot(); err != nil || len(snap.Leases) != 1 || snap.Leases[0].CeilingBytes != 1664*mib {
		t.Fatalf("while a bucket runs: leases %+v, %v; want the run's one, of 1664 MiB", snap.Leases, err)
	}
	other, err := m.Reserve(governor.Request{RunID: "other", Worktree: "/elsewhere", DataRoot: t.TempDir(), OwnerPID: os.Getpid(), CeilingBytes: (7168 - 1664) * mib})
	if err != nil {
		t.Fatalf("a second run could not reserve the 5504 MiB the list step's lease no longer holds: %v", err)
	}
	if err := m.Release(other); err != nil {
		t.Fatal(err)
	}
	r.signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("the run did not end after the signal")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"the list steps: 7168 MiB", "released the list steps", "the buckets: 1664 MiB", "released the buckets"}; !slices.Equal(leases, want) {
		t.Errorf("leases %q; want %q", leases, want)
	}
	if snap, err := m.Snapshot(); err != nil || len(snap.Leases) != 0 {
		t.Errorf("after the run: leases %+v, %v; want none", snap.Leases, err)
	}
}

// testGovernor is a governor whose host has available bytes and keeps a
// floor of 1 GiB.
func testGovernor(t *testing.T, available uint64) *governor.Manager {
	t.Helper()
	m, err := governor.New(governor.Options{Dir: t.TempDir(), AvailableFloorBytes: 1 << 30, Memory: &fixedMemory{available: available}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// fixedGovernor is testGovernor, with a runner that asks it every 10ms,
// and the request a run makes.
func fixedGovernor(t *testing.T, available uint64) (*governor.Manager, *runner, *lockedWriter, governor.Request) {
	t.Helper()
	stderr := &lockedWriter{}
	r := must(newRunner("go", &lockedWriter{}, stderr))
	t.Cleanup(r.close)
	r.admitPoll = 10 * time.Millisecond
	return testGovernor(t, available), r, stderr, governor.Request{RunID: "go-test", Worktree: t.TempDir(), DataRoot: t.TempDir(), OwnerPID: os.Getpid()}
}

// TestAZeroGrantWaitsUntilAHolderReleases has another run hold all but
// half a unit above the floor: the run says once why it waits and starts
// no bucket until that run releases, then runs as many as fit.
func TestAZeroGrantWaitsUntilAHolderReleases(t *testing.T) {
	t.Parallel()
	const unit, floor = 100 << 20, 1 << 30
	m, r, stderr, req := fixedGovernor(t, floor+5*unit/2)
	holder, err := m.Reserve(governor.Request{RunID: "holder", Worktree: "/elsewhere", DataRoot: t.TempDir(), OwnerPID: os.Getpid(), CeilingBytes: 2 * unit})
	if err != nil {
		t.Fatal(err)
	}
	type grant struct {
		w       int
		release func() error
		err     error
	}
	granted := make(chan grant, 1)
	go func() {
		w, release, err := r.reserve(m, req, bucketPhase, []uint64{unit, 2 * unit, 3 * unit, 4 * unit})
		granted <- grant{w, release, err}
	}()
	deadline := time.Now().Add(time.Minute)
	for !strings.Contains(stderr.String(), "waiting for memory") {
		if time.Now().After(deadline) {
			t.Fatal("the run never said it was waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case g := <-granted:
		t.Fatalf("granted %d (%v) while nothing fits; want a wait", g.w, g.err)
	case <-time.After(100 * time.Millisecond):
	}
	want := "ao-gotest: waiting for memory for the buckets: one bucket needs 100 MiB and 50 MiB is free: 1274 MiB available less the 1024 MiB floor the host keeps and the 200 MiB other runs reserved (holder in /elsewhere holds 200 MiB)\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr %q; want %q, once", got, want)
	}
	if err := m.Release(holder); err != nil {
		t.Fatal(err)
	}
	g := <-granted
	if g.err != nil || g.w != 2 {
		t.Fatalf("granted %d, %v once the holder released; want the 2 units that fit", g.w, g.err)
	}
	if err := g.release(); err != nil {
		t.Fatal(err)
	}
}

// TestARefusalPastItsBoundFailsWithTheReason: a run that nothing frees
// for fails with the reason once its wait is over, holding no lease.
func TestARefusalPastItsBoundFailsWithTheReason(t *testing.T) {
	t.Parallel()
	const unit, floor = 100 << 20, 1 << 30
	m, r, stderr, req := fixedGovernor(t, floor+unit/2)
	r.admitWait = 50 * time.Millisecond
	w, _, err := r.reserve(m, req, bucketPhase, []uint64{unit})
	if w != 0 || err == nil || !strings.HasPrefix(err.Error(), "no memory for the buckets after 50ms: one bucket needs 100 MiB and 50 MiB is free") {
		t.Fatalf("reserve = %d, %v; want the refusal", w, err)
	}
	if got := strings.Count(stderr.String(), "waiting for memory"); got != 1 {
		t.Fatalf("said it was waiting %d times; want once:\n%s", got, stderr.String())
	}
	if snap, err := m.Snapshot(); err != nil || len(snap.Leases) != 0 {
		t.Fatalf("leases %v, %v; want none", snap.Leases, err)
	}
}

// TestASignalEndsTheWaitForMemory: a signal while the run waits ends the
// wait at once.
func TestASignalEndsTheWaitForMemory(t *testing.T) {
	t.Parallel()
	const unit, floor = 100 << 20, 1 << 30
	m, r, _, req := fixedGovernor(t, floor)
	done := make(chan error, 1)
	go func() {
		_, _, err := r.reserve(m, req, bucketPhase, []uint64{unit})
		done <- err
	}()
	r.signal(syscall.SIGTERM)
	select {
	case err := <-done:
		if !errors.Is(err, errStopped) {
			t.Fatalf("reserve = %v; want it stopped", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the wait went on after the signal")
	}
}

type fixedMemory struct{ available uint64 }

func (f *fixedMemory) AvailableMemory() (uint64, error) { return f.available, nil }
