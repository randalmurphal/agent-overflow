package governor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

type scriptedMemory struct {
	mu      sync.Mutex
	values  []uint64
	last    uint64
	readErr error
}

func (f *scriptedMemory) AvailableMemory() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return 0, f.readErr
	}
	if len(f.values) != 0 {
		f.last = f.values[0]
		f.values = f.values[1:]
	}
	return f.last, nil
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

func testManager(t *testing.T, memory MemoryReader, processes *fakeProcesses, now *time.Time) *Manager {
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

func TestReservationsCoordinateAcrossManagers(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("host locking is intentionally unsupported on this platform")
	}
	now := time.Unix(100, 0)
	dir := filepath.Join(t.TempDir(), "global")
	proc := &fakeProcesses{states: map[int]ProcessState{
		42: {Alive: true, BirthID: "boot-1"},
		43: {Alive: true, BirthID: "boot-2"},
	}}
	newManager := func(memory *fakeMemory) *Manager {
		m, err := New(Options{Dir: dir, DefaultCeilingBytes: 300, AvailableFloorBytes: 100, LeaseTTL: time.Minute, Memory: memory, Processes: proc, Clock: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m1 := newManager(&fakeMemory{available: 500})
	m2 := newManager(&fakeMemory{available: 500})
	first, second := request(), request()
	first.RunID, first.Worktree, first.DataRoot, first.OwnerPID, first.OwnerBirthID = "run-1", "/repo/wt-1", "/tmp/ao-root-1", 42, "boot-1"
	second.RunID, second.Worktree, second.DataRoot, second.OwnerPID, second.OwnerBirthID = "run-2", "/repo/wt-2", "/tmp/ao-root-2", 43, "boot-2"
	results := make(chan error, 2)
	go func() { _, err := m1.Reserve(first); results <- err }()
	go func() { _, err := m2.Reserve(second); results <- err }()
	var reserveErrs [2]error
	reserveErrs[0], reserveErrs[1] = <-results, <-results
	var nilErrs, capacityErrors int
	for _, reserveErr := range reserveErrs {
		if reserveErr == nil {
			nilErrs++
		} else if errors.Is(reserveErr, ErrCapacityExceeded) {
			capacityErrors++
		} else {
			t.Fatalf("cross-manager reserve err = %v", reserveErr)
		}
	}
	if nilErrs != 1 || capacityErrors != 1 {
		t.Fatalf("cross-manager reserve results = nil:%d capacity:%d, want one each", nilErrs, capacityErrors)
	}
	// Re-read the durable state. Two successful 300-byte claims would leave
	// less than the 100-byte floor, which the shared lock must prevent.
	snapshot, err := m1.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	successes := len(snapshot.Leases)
	if successes != 1 {
		t.Fatalf("cross-manager reservations = %d, want 1", successes)
	}
	if snapshot.ReservedBytes != 300 {
		t.Fatalf("reserved bytes = %d, want 300", snapshot.ReservedBytes)
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

func TestDeadLeaseReleasesCapacityBeforeTTL(t *testing.T) {
	now := time.Unix(100, 0)
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}
	m := testManager(t, &fakeMemory{available: 500}, proc, &now)
	if _, err := m.Reserve(request()); err != nil {
		t.Fatal(err)
	}
	proc.mu.Lock()
	proc.states[42] = ProcessState{}
	proc.mu.Unlock()
	snapshot, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Leases) != 0 {
		t.Fatalf("dead lease remains before TTL: %+v", snapshot.Leases)
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

func TestPIDReusePrunesExpiredLease(t *testing.T) {
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
	if len(snap.Leases) != 0 {
		t.Fatalf("expired lease for a reused PID remains: %+v", snap.Leases)
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

func TestMonitorEmitsAvailableFloorAndStops(t *testing.T) {
	now := time.Unix(100, 0)
	mem := &scriptedMemory{values: []uint64{500, 500, 99, 99}, last: 500}
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}
	m := testManager(t, mem, proc, &now)
	lease, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 1)
	done := make(chan error, 1)
	go func() {
		done <- m.Monitor(ctx, lease, time.Millisecond, fakeRSS{rss: 1}, func(event Event) {
			events <- event
			cancel()
		})
	}()
	select {
	case event := <-events:
		if event.Reason != ReasonAvailableFloor || event.AvailableBytes != 99 || event.AvailableFloorBytes != 100 {
			t.Fatalf("event = %+v", event)
		}
		if event.RSSBytes != 0 {
			t.Fatalf("host pressure event RSS = %d, want 0", event.RSSBytes)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor emitted no host pressure event")
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

func TestMonitorHostPressureWinsWhenRSSReadFails(t *testing.T) {
	now := time.Unix(100, 0)
	mem := &fakeMemory{available: 500}
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}
	m := testManager(t, mem, proc, &now)
	lease, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	mem.mu.Lock()
	mem.available = 99
	mem.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 1)
	done := make(chan error, 1)
	go func() {
		done <- m.Monitor(ctx, lease, time.Millisecond, fakeRSS{err: errors.New("child disappeared")}, func(event Event) {
			events <- event
			cancel()
		})
	}()
	select {
	case event := <-events:
		if event.Reason != ReasonAvailableFloor {
			t.Fatalf("event reason = %q, want %q", event.Reason, ReasonAvailableFloor)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor emitted no host pressure event")
	}
	if err := <-done; err != nil {
		t.Fatalf("monitor returned after host pressure event: %v", err)
	}
}

func TestMonitorDoesNotStormOnPersistentPressure(t *testing.T) {
	now := time.Unix(100, 0)
	mem := &fakeMemory{available: 500}
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}
	m := testManager(t, mem, proc, &now)
	lease, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	mem.mu.Lock()
	mem.available = 99
	mem.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	count := 0
	done := make(chan error, 1)
	go func() {
		done <- m.Monitor(ctx, lease, time.Millisecond, fakeRSS{rss: 1}, func(Event) {
			mu.Lock()
			count++
			mu.Unlock()
		})
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("persistent host pressure emitted %d events, want 1", count)
	}
}

func TestMonitorNoFalseTriggerAtThresholds(t *testing.T) {
	now := time.Unix(100, 0)
	mem := &fakeMemory{available: 500}
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}
	m := testManager(t, mem, proc, &now)
	lease, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	mem.mu.Lock()
	mem.available = 100
	mem.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	count := 0
	done := make(chan error, 1)
	go func() {
		done <- m.Monitor(ctx, lease, time.Millisecond, fakeRSS{rss: 100}, func(Event) {
			mu.Lock()
			count++
			mu.Unlock()
		})
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("threshold values emitted %d events, want 0", count)
	}
}

func TestMonitorCancellationBeforeSample(t *testing.T) {
	now := time.Unix(100, 0)
	m := testManager(t, &fakeMemory{available: 500}, &fakeProcesses{states: map[int]ProcessState{}}, &now)
	lease, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- m.Monitor(ctx, lease, time.Hour, fakeRSS{rss: 1}, func(Event) {}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not honor cancellation")
	}
}

func TestMonitorReportsOwnerProbeError(t *testing.T) {
	now := time.Unix(100, 0)
	proc := &fakeProcesses{states: map[int]ProcessState{42: {Alive: true, BirthID: "boot-1"}}}
	m := testManager(t, &fakeMemory{available: 500}, proc, &now)
	lease, err := m.Reserve(request())
	if err != nil {
		t.Fatal(err)
	}
	proc.mu.Lock()
	proc.err = errors.New("identity probe failed")
	proc.mu.Unlock()
	errCh := make(chan error, 1)
	go func() {
		errCh <- m.Monitor(context.Background(), lease, time.Millisecond, fakeRSS{rss: 1}, func(Event) {})
	}()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "monitor owner") {
			t.Fatalf("monitor error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not report owner probe error")
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
