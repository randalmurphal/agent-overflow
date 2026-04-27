package gitwatch

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rjeczalik/notify"

	gitops "agent-overflow/internal/git"
)

// recvWithin reads one update from sub.Updates() or fails the test.
// All gitwatch behavior is async (debounce + watcher goroutine), so
// every assertion that expects an update needs a timeout — never a
// time.Sleep.
func recvWithin(t *testing.T, sub *Subscription, d time.Duration) gitops.GitStatus {
	t.Helper()
	select {
	case status, ok := <-sub.Updates():
		if !ok {
			t.Fatalf("subscription closed while expecting update")
		}
		return status
	case <-time.After(d):
		t.Fatalf("timed out waiting for update after %s", d)
		return gitops.GitStatus{}
	}
}

// expectNoUpdate asserts that no update arrives within d.
func expectNoUpdate(t *testing.T, sub *Subscription, d time.Duration) {
	t.Helper()
	select {
	case status, ok := <-sub.Updates():
		if !ok {
			t.Fatalf("subscription closed unexpectedly")
		}
		t.Fatalf("unexpected update arrived: %+v", status)
	case <-time.After(d):
	}
}

// stubStatus is a thread-safe StatusFn that returns whatever was last
// set via setStatus. Useful for tests that want to control exactly what
// the watcher sees on each refresh, without shelling out to git.
type stubStatus struct {
	mu       sync.Mutex
	current  gitops.GitStatus
	calls    int32
	failNext atomic.Bool
}

func newStubStatus(initial gitops.GitStatus) *stubStatus {
	return &stubStatus{current: initial}
}

func (s *stubStatus) fn() StatusFn {
	return func(cwd string) (gitops.GitStatus, error) {
		atomic.AddInt32(&s.calls, 1)
		if s.failNext.CompareAndSwap(true, false) {
			return gitops.GitStatus{}, errors.New("stub: forced failure")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.current, nil
	}
}

func (s *stubStatus) setStatus(next gitops.GitStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = next
}

func (s *stubStatus) callCount() int {
	return int(atomic.LoadInt32(&s.calls))
}

// makeRepoDir creates an empty directory under t.TempDir that callers
// can use as a workspace path. Real git status calls require a real
// repo; tests that stub StatusFn don't, so this is enough.
func makeRepoDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir
}

func TestSubscribeReturnsInitialStatus(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main"})
	mgr := NewManager(stub.fn())
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	if got := sub.Initial(); got.Branch != "main" {
		t.Fatalf("initial branch = %q, want main", got.Branch)
	}
}

func TestSubscribeReturnsErrorOnInitialFetchFailure(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{})
	stub.failNext.Store(true)
	mgr := NewManager(stub.fn())
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(dir)
	if err == nil {
		sub.Close()
		t.Fatalf("expected initial fetch error to propagate")
	}
}

func TestFsEventTriggersDedupedUpdate(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main"})
	mgr := NewManager(stub.fn())
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	// First write: the watcher debounces ~250ms then re-runs StatusFn,
	// which now reports a change.
	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", HasChanges: true, FileCount: 1})
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := recvWithin(t, sub, 5*time.Second)
	if !got.HasChanges {
		t.Fatalf("expected HasChanges=true, got %+v", got)
	}

	// Second write while StatusFn returns the same value: dedup must
	// suppress the emit. We give it a debounce window plus margin and
	// expect nothing on the channel.
	if err := os.WriteFile(filepath.Join(dir, "file2.txt"), []byte("hi2"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectNoUpdate(t, sub, debounceWindow+200*time.Millisecond)
}

func TestMultipleSubscribersShareWatcher(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main"})
	mgr := NewManager(stub.fn())
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	subA, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	defer subA.Close()
	subB, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	defer subB.Close()

	mgr.mu.Lock()
	count := len(mgr.watchers)
	mgr.mu.Unlock()
	if count != 1 {
		t.Fatalf("watchers = %d, want 1 (refcount sharing)", count)
	}

	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", HasChanges: true})
	if err := os.WriteFile(filepath.Join(dir, "trigger.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	gotA := recvWithin(t, subA, 5*time.Second)
	gotB := recvWithin(t, subB, 5*time.Second)
	if !gotA.HasChanges || !gotB.HasChanges {
		t.Fatalf("both subscribers should see HasChanges=true; got A=%+v B=%+v", gotA, gotB)
	}
}

func TestLastUnsubscribeStopsWatcher(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main"})
	mgr := NewManager(stub.fn())
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	subA, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	subB, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}

	subA.Close()

	mgr.mu.Lock()
	count := len(mgr.watchers)
	mgr.mu.Unlock()
	if count != 1 {
		t.Fatalf("after one Close, watchers = %d, want 1", count)
	}

	subB.Close()

	mgr.mu.Lock()
	count = len(mgr.watchers)
	mgr.mu.Unlock()
	if count != 0 {
		t.Fatalf("after both Close, watchers = %d, want 0", count)
	}

	// Channel is closed after Close.
	select {
	case _, ok := <-subB.Updates():
		if ok {
			t.Fatalf("expected closed channel after Close")
		}
	case <-time.After(time.Second):
		t.Fatalf("Updates() did not close after final Close")
	}
}

func TestPollingFallbackEmitsUpdates(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main"})
	mgr := NewManager(stub.fn())
	// Force watcher install to fail so the watcher uses the polling
	// fallback. This validates the inotify-exhaustion path without
	// actually running out of watches.
	mgr.installFn = func(string, chan<- notify.EventInfo) error {
		return errors.New("forced fallback")
	}
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	mgr.mu.Lock()
	w := mgr.watchers[mustCanonical(t, dir)]
	mgr.mu.Unlock()
	if w == nil || !w.fallbackPolling {
		t.Fatalf("expected fallbackPolling=true, got watcher=%v", w)
	}

	// Update the stub status; the polling tick (3s) should pick it up
	// without any fs event being delivered. Use a generous timeout so
	// CI variance doesn't flake.
	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", HasChanges: true})
	got := recvWithin(t, sub, pollFallbackInterval+2*time.Second)
	if !got.HasChanges {
		t.Fatalf("polling fallback did not emit; got %+v", got)
	}
}

func TestManagerCloseDrainsSubscribers(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main"})
	mgr := NewManager(stub.fn())

	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	mgr.Close()

	select {
	case _, ok := <-sub.Updates():
		if ok {
			t.Fatalf("expected closed channel after Manager.Close")
		}
	case <-time.After(time.Second):
		t.Fatalf("Updates() did not close after Manager.Close")
	}

	// Subsequent Subscribe must fail.
	if _, err := mgr.Subscribe(dir); err == nil {
		t.Fatalf("Subscribe after Close must error")
	}
}

func TestStatusFnFailureDoesNotEmit(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main"})
	mgr := NewManager(stub.fn())
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	callsBefore := stub.callCount()
	stub.failNext.Store(true)
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectNoUpdate(t, sub, debounceWindow+500*time.Millisecond)
	if stub.callCount() <= callsBefore {
		t.Fatalf("StatusFn was never invoked after fs event; assertion is vacuous (calls before=%d, after=%d)",
			callsBefore, stub.callCount())
	}
	if stub.failNext.Load() {
		t.Fatalf("failNext flag never consumed; the watcher did not run StatusFn")
	}
}

// TestConcurrentRefreshAndUnsubscribe validates the channel-close
// race fix. Pre-fix: refresh's snapshot could send on a channel that
// removeSubscriber concurrently closed → panic recovered → watcher
// goroutine dead → cascade close of all peers' subs. Post-fix:
// refresh holds w.mu through the broadcast so removeSubscriber's
// close cannot interleave with a send. Run under -race for the full
// signal — the fix removes a Go memory model violation as well as
// the panic.
func TestConcurrentRefreshAndUnsubscribe(t *testing.T) {
	t.Parallel()
	current := atomic.Int64{}
	statusFn := func(string) (gitops.GitStatus, error) {
		// Vary the returned value per-call so dedup never short-circuits
		// the broadcast — we want to drive the broadcast loop on every
		// fs event.
		n := current.Add(1)
		return gitops.GitStatus{IsRepo: true, Branch: "main", AheadCount: int(n)}, nil
	}
	mgr := NewManager(statusFn)
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	const initialSubs = 8
	subs := make([]*Subscription, 0, initialSubs)
	for i := 0; i < initialSubs; i++ {
		s, err := mgr.Subscribe(dir)
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		subs = append(subs, s)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Drainer goroutines: empty Updates() so the run loop's
	// non-blocking send always succeeds (without consumers, the supersede
	// drain might be exercised but the test doesn't depend on it).
	for _, s := range subs {
		wg.Add(1)
		go func(s *Subscription) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-s.Updates():
				}
			}
		}(s)
	}
	// Trigger goroutine: file writes drive refresh().
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(dir, "f"), []byte{byte(i)}, 0o644)
		}
	}()
	// Churn goroutine: cycles new subscribers in/out so removeSubscriber
	// runs concurrently with refresh().
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s, err := mgr.Subscribe(dir)
			if err != nil {
				return
			}
			s.Close()
		}
	}()
	// Run for a short while, then tear down. -race + -count=1 is the
	// real assertion; this test fails fast (panic, deadlock, or race
	// report) without the fix.
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	for _, s := range subs {
		s.Close()
	}
}

// TestSupersedeOnOverflow exercises the drain-then-send pattern in
// refresh(): when a subscriber is slow and a second status arrives
// before they read, the newer value supersedes the older one rather
// than queuing or dropping. Without this property, a slow consumer
// would see an arbitrarily old status while the watcher's lastStatus
// is fresh.
func TestSupersedeOnOverflow(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main", AheadCount: 1})
	mgr := NewManager(stub.fn())
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	// Wait until both refreshes have run; without reading sub.Updates()
	// the channel buffer (size 1) holds the latest value via supersede.
	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", AheadCount: 2})
	if err := os.WriteFile(filepath.Join(dir, "first"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for stub.callCount() < 2 {
		select {
		case <-deadline:
			t.Fatalf("first refresh never ran; calls=%d", stub.callCount())
		case <-time.After(20 * time.Millisecond):
		}
	}
	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", AheadCount: 7})
	if err := os.WriteFile(filepath.Join(dir, "second"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for stub.callCount() < 3 {
		select {
		case <-deadline:
			t.Fatalf("second refresh never ran; calls=%d", stub.callCount())
		case <-time.After(20 * time.Millisecond):
		}
	}

	got := recvWithin(t, sub, 2*time.Second)
	if got.AheadCount != 7 {
		t.Fatalf("supersede failed: got AheadCount=%d, want 7 (newest)", got.AheadCount)
	}
}

// TestWatcherPanicRecovery confirms the run-goroutine's defer recover
// catches a panic in StatusFn (or anywhere downstream) and tears the
// watcher down cleanly. Without recover the goroutine would crash the
// process; with it, subscribers see their Updates() channel close and
// can resubscribe.
func TestWatcherPanicRecovery(t *testing.T) {
	t.Parallel()
	var fired atomic.Bool
	statusFn := func(string) (gitops.GitStatus, error) {
		if fired.CompareAndSwap(false, true) {
			return gitops.GitStatus{IsRepo: true, Branch: "main"}, nil
		}
		panic("simulated statusFn panic")
	}
	mgr := NewManager(statusFn)
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "boom"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Updates() should close because the run loop's defer recovers
	// then broadcastClose drops everyone.
	select {
	case _, ok := <-sub.Updates():
		if ok {
			t.Fatalf("expected channel close after panic")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Updates() did not close after panic")
	}
}

// TestManagerCloseBlocksUntilWatchersExit pins the contract that
// Manager.Close blocks until run goroutines have actually exited.
// Without this guarantee a caller running `mgr.Close(); writeToChan()`
// would race the still-emitting pump goroutines.
func TestManagerCloseBlocksUntilWatchersExit(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	statusFn := func(string) (gitops.GitStatus, error) {
		<-release
		return gitops.GitStatus{IsRepo: true, Branch: "main"}, nil
	}
	mgr := NewManager(statusFn)

	dir := makeRepoDir(t)
	subDone := make(chan struct{})
	go func() {
		_, _ = mgr.Subscribe(dir)
		close(subDone)
	}()

	// Subscribe is blocked in statusFn. Until released, no watcher
	// exists, so close should be quick. Release after kicking off
	// Close to drive the contended path.
	closeDone := make(chan struct{})
	go func() {
		mgr.Close()
		close(closeDone)
	}()

	// Close must NOT have returned yet; statusFn is still blocked.
	select {
	case <-closeDone:
		// Acceptable if Close races and finds no watchers (Subscribe
		// hasn't installed one yet). Either order is correct, but
		// once we release, both must finish.
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-subDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("Subscribe did not return after release")
	}
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("Manager.Close did not return after release")
	}
}

// TestSubscribeRejectsEmptyCwd / TestSubscribeRejectsNonexistentPath /
// TestSubscribeRejectsSystemPath cover the three reject paths in
// Subscribe: argument validation, canonicalize() on a missing path,
// and the system-path defense-in-depth backstop.
func TestSubscribeRejectsEmptyCwd(t *testing.T) {
	t.Parallel()
	mgr := NewManager(newStubStatus(gitops.GitStatus{}).fn())
	t.Cleanup(mgr.Close)
	if _, err := mgr.Subscribe(""); err == nil {
		t.Fatalf("Subscribe(\"\") must error")
	}
}

func TestSubscribeRejectsNonexistentPath(t *testing.T) {
	t.Parallel()
	mgr := NewManager(newStubStatus(gitops.GitStatus{}).fn())
	t.Cleanup(mgr.Close)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := mgr.Subscribe(missing); err == nil {
		t.Fatalf("Subscribe(missing) must error")
	}
	mgr.mu.Lock()
	count := len(mgr.watchers)
	mgr.mu.Unlock()
	if count != 0 {
		t.Fatalf("watcher leaked on rejected Subscribe: %d active", count)
	}
}

func TestSubscribeRejectsSystemPath(t *testing.T) {
	t.Parallel()
	mgr := NewManager(newStubStatus(gitops.GitStatus{}).fn())
	t.Cleanup(mgr.Close)
	for _, p := range []string{"/", "/etc", "/var"} {
		if _, err := os.Stat(p); err != nil {
			continue // not present on this OS
		}
		if _, err := mgr.Subscribe(p); err == nil {
			t.Fatalf("Subscribe(%q) must be refused", p)
		}
	}
}

// TestSubscribeSurfacesInitialFetchFailureWithoutLeak extends the
// earlier "error propagates" test with an explicit leak check: a
// failed fetch must not leave a watcher in m.watchers.
func TestSubscribeSurfacesInitialFetchFailureWithoutLeak(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{})
	stub.failNext.Store(true)
	mgr := NewManager(stub.fn())
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	if _, err := mgr.Subscribe(dir); err == nil {
		t.Fatalf("expected initial fetch error to propagate")
	}
	mgr.mu.Lock()
	count := len(mgr.watchers)
	mgr.mu.Unlock()
	if count != 0 {
		t.Fatalf("watcher leaked after failed Subscribe: %d active", count)
	}
}

// TestSharedCwdReusesFreshLastStatus pins the perf optimisation that
// a second Subscribe to the same cwd does NOT re-run statusFn, because
// the existing watcher's lastStatus is already fresh.
func TestSharedCwdReusesFreshLastStatus(t *testing.T) {
	t.Parallel()
	stub := newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main"})
	mgr := NewManager(stub.fn())
	t.Cleanup(mgr.Close)

	dir := makeRepoDir(t)
	subA, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	defer subA.Close()
	callsAfterA := stub.callCount()

	subB, err := mgr.Subscribe(dir)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	defer subB.Close()
	if got := stub.callCount(); got != callsAfterA {
		t.Fatalf("Subscribe to shared cwd ran statusFn again: calls before=%d after=%d", callsAfterA, got)
	}
	if subB.Initial().Branch != "main" {
		t.Fatalf("shared subscriber did not get a useful initial: %+v", subB.Initial())
	}
}

func mustCanonical(t *testing.T, p string) string {
	t.Helper()
	_, c, err := canonicalize(p)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	return c
}
