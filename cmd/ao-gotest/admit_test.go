package main

import (
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/harness/governor"
)

// TestReservationTotalsCapAtOnePackagesBuckets: a run reserves for at most
// a bucket per core and at most one package's buckets, the largest first,
// and k buckets at once hold the larger of the largest list step and the k
// buckets together, since no bucket runs beside a list step.
func TestReservationTotalsCapAtOnePackagesBuckets(t *testing.T) {
	t.Parallel()
	app, store := split{"example/app", 1664 << 20, 7168 << 20}, split{"example/store", 640 << 20, 640 << 20}
	mib := func(totals ...uint64) []uint64 {
		for k := range totals {
			totals[k] <<= 20
		}
		return totals
	}
	for _, tc := range []struct {
		declared []split
		cores    int
		want     []uint64
	}{
		{[]split{app, store}, 24, mib(7168, 7168, 7168, 7168, 8320, 9984, 11648, 13312)},
		{[]split{store, app}, 24, mib(7168, 7168, 7168, 7168, 8320, 9984, 11648, 13312)},
		{[]split{app, store}, 4, mib(7168, 7168, 7168, 7168)},
		{[]split{app, store}, 1, mib(7168)},
		{[]split{store}, 24, mib(640, 1280, 1920, 2560, 3200, 3840, 4480, 5120)},
		{[]split{{"example/small", 100 << 20, 150 << 20}}, 2, mib(150, 200)},
	} {
		if got := reservationTotals(tc.declared, tc.cores); !slices.Equal(got, tc.want) {
			t.Errorf("%v on %d cores: totals %v; want %v", tc.declared, tc.cores, got, tc.want)
		}
	}
}

// fixedGovernor is a governor whose host has available bytes, with a
// runner that asks it every 10ms, and the request a run makes.
func fixedGovernor(t *testing.T, available uint64) (*fixedMemory, *governor.Manager, *runner, *lockedWriter, governor.Request) {
	t.Helper()
	memory := &fixedMemory{available: available}
	m, err := governor.New(governor.Options{Dir: t.TempDir(), AvailableFloorBytes: 1 << 30, Memory: memory})
	if err != nil {
		t.Fatal(err)
	}
	stderr := &lockedWriter{}
	r := must(newRunner("go", &lockedWriter{}, stderr))
	t.Cleanup(r.close)
	r.admitPoll = 10 * time.Millisecond
	return memory, m, r, stderr, governor.Request{RunID: "go-test", Worktree: t.TempDir(), DataRoot: t.TempDir(), OwnerPID: os.Getpid()}
}

// TestAZeroGrantWaitsUntilAHolderReleases has another run hold all but
// half a unit above the floor: the run says once why it waits and starts
// no bucket until that run releases, then runs as many as fit.
func TestAZeroGrantWaitsUntilAHolderReleases(t *testing.T) {
	t.Parallel()
	const unit, floor = 100 << 20, 1 << 30
	_, m, r, stderr, req := fixedGovernor(t, floor+5*unit/2)
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
		w, release, err := r.reserve(m, req, []uint64{unit, 2 * unit, 3 * unit, 4 * unit})
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
	want := "ao-gotest: waiting for memory for the buckets: one bucket and the list steps before it need 100 MiB and 50 MiB is free: 1274 MiB available less the 1024 MiB floor the host keeps and the 200 MiB other runs reserved (holder in /elsewhere holds 200 MiB)\n"
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
	_, m, r, stderr, req := fixedGovernor(t, floor+unit/2)
	r.admitWait = 50 * time.Millisecond
	w, _, err := r.reserve(m, req, []uint64{unit})
	if w != 0 || err == nil || !strings.HasPrefix(err.Error(), "no memory for the buckets after 50ms: one bucket and the list steps before it need 100 MiB and 50 MiB is free") {
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
	_, m, r, _, req := fixedGovernor(t, floor)
	done := make(chan error, 1)
	go func() {
		_, _, err := r.reserve(m, req, []uint64{unit})
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
