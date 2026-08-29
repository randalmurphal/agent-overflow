package governor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMemory struct {
	mu        sync.Mutex
	available uint64
	err       error
}

func (f *fakeMemory) AvailableMemory() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available, f.err
}

type fakeProcesses struct {
	mu     sync.Mutex
	states map[int]ProcessState
	err    error
}

func (f *fakeProcesses) State(pid int) (ProcessState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return ProcessState{}, f.err
	}
	return f.states[pid], nil
}

type fakeRSS struct {
	rss uint64
	err error
}

func (f fakeRSS) RSS(int) (uint64, error) { return f.rss, f.err }

func testManager(t *testing.T, memory *fakeMemory, processes *fakeProcesses, now *time.Time) *Manager {
	t.Helper()
	if _, ok := processes.states[42]; !ok {
		processes.states[42] = ProcessState{Alive: true, BirthID: "boot-1"}
	}
	m, err := New(Options{Dir: filepath.Join(t.TempDir(), "global"), DefaultCeilingBytes: 100, AvailableFloorBytes: 100, LeaseTTL: time.Minute, Memory: memory, Processes: processes, Clock: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func request() Request {
	return Request{RunID: "run-1", Worktree: "/repo/wt", DataRoot: "/tmp/ao-root", OwnerPID: 42, OwnerBirthID: "boot-1"}
}

func TestReserveUsesDefaultAndAggregatesCapacity(t *testing.T) {
	now := time.Unix(100, 0)
	mem := &fakeMemory{available: 500}
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}, 43: {Alive: true, BirthID: "boot-2"}, 44: {Alive: true, BirthID: "boot-3"}}}
	m := testManager(t, mem, proc, &now)
	l1, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	if l1.CeilingBytes != 100 || !l1.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("lease = %+v", l1)
	}
	second := request()
	second.RunID = "run-2"
	second.OwnerPID = 43
	second.OwnerBirthID = "boot-2"
	if _, err := m.Reserve(second); err != nil {
		t.Fatal(err)
	}
	third := request()
	third.RunID = "run-3"
	third.OwnerPID = 44
	third.OwnerBirthID = "boot-3"
	third.CeilingBytes = 201
	if _, err := m.Reserve(third); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("third reserve err = %v, want capacity refusal", err)
	}
	if _, err := m.Reserve(request()); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("duplicate err = %v", err)
	}
}

func TestReserveRefusesWhenAvailableFloorWouldBeBreached(t *testing.T) {
	now := time.Unix(100, 0)
	m := testManager(t, &fakeMemory{available: 99}, &fakeProcesses{states: map[int]ProcessState{}}, &now)
	if _, err := m.Reserve(request()); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestExpiredLeasePrunesOnlyVerifiedDeadOwner(t *testing.T) {
	now := time.Unix(100, 0)
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}
	m := testManager(t, &fakeMemory{available: 500}, proc, &now)
	lease, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if snap, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	} else if len(snap.Leases) != 1 {
		t.Fatal("expired live lease was pruned")
	}
	proc.mu.Lock()
	proc.states[42] = ProcessState{}
	proc.mu.Unlock()
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Leases) != 0 {
		t.Fatalf("dead lease remains: %+v", snap.Leases)
	}
	if err := m.Release(lease); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("release pruned lease err = %v", err)
	}
}

func TestExpiredLeaseWithProbeErrorIsPreserved(t *testing.T) {
	now := time.Unix(100, 0)
	proc := &fakeProcesses{states: map[int]ProcessState{}}
	m := testManager(t, &fakeMemory{available: 500}, proc, &now)
	if _, err := m.Reserve(request()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	proc.err = errors.New("probe unavailable")
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Leases) != 1 {
		t.Fatal("probe failure dropped lease")
	}
}

func TestReleaseRequiresBirthIdentity(t *testing.T) {
	now := time.Unix(100, 0)
	m := testManager(t, &fakeMemory{available: 500}, &fakeProcesses{states: map[int]ProcessState{}}, &now)
	lease, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	lease.OwnerBirthID = "different-process"
	if err := m.Release(lease); !errors.Is(err, ErrLeaseOwnerMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestReserveRejectsMismatchedBirthIdentity(t *testing.T) {
	now := time.Unix(100, 0)
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "actual"}}}
	m := testManager(t, &fakeMemory{available: 500}, proc, &now)
	req := request()
	req.OwnerBirthID = "old-pid-instance"
	if _, err := m.Reserve(req); !errors.Is(err, ErrLeaseOwnerMismatch) {
		t.Fatalf("err = %v, want owner mismatch", err)
	}
}

func TestPIDReuseDoesNotPruneExpiredLease(t *testing.T) {
	now := time.Unix(100, 0)
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}
	m := testManager(t, &fakeMemory{available: 500}, proc, &now)
	if _, err := m.Reserve(request()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	proc.mu.Lock()
	proc.states[42] = ProcessState{Alive: true, BirthID: "different-boot"}
	proc.mu.Unlock()
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Leases) != 1 {
		t.Fatal("expired lease for a reused PID was pruned")
	}
}

func TestMonitorEmitsSafetyCeilingAndStops(t *testing.T) {
	now := time.Unix(100, 0)
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}
	m := testManager(t, &fakeMemory{available: 500}, proc, &now)
	lease, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 1)
	done := make(chan error, 1)
	go func() {
		done <- m.Monitor(ctx, lease, time.Millisecond, fakeRSS{rss: 101}, func(e Event) { events <- e; cancel() })
	}()
	select {
	case event := <-events:
		if event.Reason != ReasonSafetyCeiling || event.RSSBytes != 101 || event.CeilingBytes != 100 {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor emitted no safety event")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop")
	}
}

func TestStateFilesArePrivateAndOutsideWorktree(t *testing.T) {
	now := time.Unix(100, 0)
	dir := filepath.Join(t.TempDir(), "global")
	m, err := New(Options{Dir: dir, DefaultCeilingBytes: 100, AvailableFloorBytes: 100, Memory: &fakeMemory{available: 500}, Processes: &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(request()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{stateFileName, lockFileName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}
	if strings.Contains(dir, request().Worktree) {
		t.Fatal("test setup accidentally put state in worktree")
	}
}
