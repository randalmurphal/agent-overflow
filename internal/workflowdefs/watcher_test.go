package workflowdefs

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWatcherDebouncesWritesAndDiscoversProjectRename(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "workflows")
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, root, 25*time.Millisecond, &debounceClock{})

	// The first write alone is a CREATE then a WRITE, which the kernel never
	// merges, so the burst queues the debounce more than once.
	definition := filepath.Join(shared, "build.yaml")
	for index := range 3 {
		if err := os.WriteFile(definition, []byte{byte('a' + index)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	observed.expectQueued(t, 0, 2, "a write burst")
	observed.releaseOneEmit(t, "a write burst")

	outside := t.TempDir()
	incoming := filepath.Join(outside, "renamed-project")
	incomingWorkflows := filepath.Join(incoming, "workflows")
	if err := os.MkdirAll(incomingWorkflows, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomingWorkflows, "ship.yaml"), []byte("id: ship"), 0o600); err != nil {
		t.Fatal(err)
	}
	armed := observed.clock.armedCount()
	if err := os.Rename(incoming, filepath.Join(projects, "renamed-project")); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "a project renamed into place")
	observed.releaseOneEmit(t, "a project renamed into place")

	// The rename's refresh must have watched the project's workflow tree.
	armed = observed.clock.armedCount()
	if err := os.WriteFile(filepath.Join(projects, "renamed-project", "workflows", "ship.yaml"), []byte("id: changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "an edit inside the renamed project")
	observed.releaseOneEmit(t, "an edit inside the renamed project")

	armed = observed.clock.armedCount()
	noise := filepath.Join(root, "store.db-wal")
	if err := os.WriteFile(noise, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(noise, filepath.Join(root, "settings.json")); err != nil {
		t.Fatal(err)
	}
	observed.expectIgnored(t, armed, "data-root noise")
}

// TestWatcherDebouncesOnTheRealClock runs the production debounce wait. Each
// burst is written while the loop is parked, so the whole burst is queued
// before the loop handles any of it and lands inside one window.
func TestWatcherDebouncesOnTheRealClock(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "workflows")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, root, time.Second, nil)
	definition := filepath.Join(shared, "build.yaml")
	for burst := range 2 {
		why := fmt.Sprintf("write burst %d", burst+1)
		release := observed.hold(t)
		for index := range 3 {
			if err := os.WriteFile(definition, []byte{byte('a' + index)}, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		release()
		observed.waitEmit(t, why)
		observed.sync(t)
		observed.expectNoEmit(t, why+" a second time")
	}
}

// TestWatcherDiscoversAWorkflowTreeRenamedIntoTheRoot runs the production
// debounce wait over a shared workflows/ tree that arrives by rename.
func TestWatcherDiscoversAWorkflowTreeRenamedIntoTheRoot(t *testing.T) {
	root := t.TempDir()
	observed := startObservedWatcher(t, root, 25*time.Millisecond, nil)

	staged := filepath.Join(t.TempDir(), "workflows")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "ship.yaml"), []byte("id: ship"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "workflows")
	if err := os.Rename(staged, shared); err != nil {
		t.Fatal(err)
	}
	observed.waitEmit(t, "a workflows tree renamed into the root")
	observed.sync(t)
	observed.expectNoEmit(t, "a workflows tree renamed into the root a second time")

	if err := os.WriteFile(filepath.Join(shared, "ship.yaml"), []byte("id: changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.waitEmit(t, "an edit inside the renamed tree")
}

// TestWatcherCloseDropsAPendingWait closes the watcher while the production
// debounce wait is pending. Nothing may emit, and Close must leave none of
// the watcher's goroutines behind.
func TestWatcherCloseDropsAPendingWait(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "workflows")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, root, time.Hour, nil)
	if err := os.WriteFile(filepath.Join(shared, "build.yaml"), []byte("id: build"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.sync(t)
	if len(watcherGoroutines()) == 0 {
		t.Fatal("no watcher goroutine found while the watcher is open")
	}
	if err := observed.watcher.Close(); err != nil {
		t.Fatal(err)
	}
	observed.expectNoEmit(t, "closing with a pending debounce wait")
	if left := watcherGoroutines(); len(left) != 0 {
		t.Fatalf("Close left %d watcher goroutines:\n%s", len(left), strings.Join(left, "\n\n"))
	}
}

// TestWatcherCloseWaitsForTheLoop parks the loop and checks that Close does
// not return until the loop has exited.
func TestWatcherCloseWaitsForTheLoop(t *testing.T) {
	observed := startObservedWatcher(t, t.TempDir(), time.Hour, nil)
	release := observed.hold(t)
	closed := make(chan error, 1)
	go func() { closed <- observed.watcher.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while the watch loop was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the watch loop was released")
	}
}

// watcherGoroutines returns the stacks of live goroutines that run or were
// started by this package's watcher or by fsnotify. Matching stacks rather
// than counting goroutines keeps runtime and test framework goroutines out
// of the result.
func watcherGoroutines() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var found []string
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(stack, "workflowdefs.(*Watcher)") || strings.Contains(stack, "github.com/fsnotify/fsnotify") {
			found = append(found, stack)
		}
	}
	return found
}

func TestWatcherRejectsInvalidSetupAndClosesCleanly(t *testing.T) {
	if _, err := NewWatcher(filepath.Join(t.TempDir(), "missing"), time.Millisecond, func() {}); err == nil {
		t.Fatal("missing root unexpectedly accepted")
	}
	invalidRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalidRoot, "workflows"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWatcher(invalidRoot, time.Millisecond, func() {}); err == nil {
		t.Fatal("workflow file in place of directory unexpectedly accepted")
	}
	root := t.TempDir()
	watcher, err := NewWatcher(root, time.Millisecond, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// debounceClock replaces a watcher's debounce waits. No wait fires until the
// test releases them, so a burst stays inside one debounce window however
// the scheduler spaces its writes and their events.
type debounceClock struct {
	mu      sync.Mutex
	pending []chan time.Time
	armed   int
}

func (c *debounceClock) after(time.Duration) <-chan time.Time {
	wait := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, wait)
	c.armed++
	return wait
}

// armedCount reports how many debounce waits the loop has started.
func (c *debounceClock) armedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armed
}

// fireAbandoned fires every wait started so far except the latest, as if
// the clock passed the deadlines the loop has already replaced, and returns
// how many it fired.
func (c *debounceClock) fireAbandoned() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) < 2 {
		return 0
	}
	abandoned := c.pending[:len(c.pending)-1]
	for _, wait := range abandoned {
		wait <- time.Now()
	}
	c.pending = c.pending[len(c.pending)-1:]
	return len(abandoned)
}

// fireLatest fires the wait the loop started last.
func (c *debounceClock) fireLatest() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return false
	}
	c.pending[len(c.pending)-1] <- time.Now()
	c.pending = nil
	return true
}

// observedWatcher runs a Watcher and reports each handled event path, so a
// test can tell when the loop has handled every event it caused. With a
// debounceClock its waits are held; without one they are the production
// waits.
type observedWatcher struct {
	watcher *Watcher
	clock   *debounceClock
	emitted chan struct{}
	handled chan string
	next    int

	holdMu   sync.Mutex
	holdPath string
	resume   chan struct{}
}

func startObservedWatcher(t *testing.T, root string, debounce time.Duration, clock *debounceClock) *observedWatcher {
	t.Helper()
	observed := &observedWatcher{
		clock:   clock,
		emitted: make(chan struct{}, 8),
		handled: make(chan string, 64),
	}
	watcher, err := openWatcher(root, debounce, func() { observed.emitted <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	if clock != nil {
		watcher.after = clock.after
	}
	watcher.handled = func(path string) {
		// Never block the loop on the report: Close waits for it. A dropped
		// path fails a waiting sync with a timeout naming its sentinel.
		select {
		case observed.handled <- path:
		default:
		}
		observed.holdMu.Lock()
		resume := observed.resume
		parked := path == observed.holdPath
		observed.holdMu.Unlock()
		if parked {
			<-resume
		}
	}
	watcher.start()
	t.Cleanup(func() {
		if err := watcher.Close(); err != nil {
			t.Errorf("close watcher: %v", err)
		}
	})
	observed.watcher = watcher
	return observed
}

// sync writes a sentinel file at the data root, which the watcher ignores,
// and waits until the loop has handled it. The kernel queues a change's
// events before the syscall returns, and the loop handles them in delivery
// order on one goroutine, so every event from an earlier change has been
// handled too.
func (o *observedWatcher) sync(t *testing.T) {
	t.Helper()
	o.waitHandled(t, o.writeSentinel(t))
}

// hold parks the loop on a sentinel until the returned release is called,
// so changes made meanwhile are all queued before the loop handles any. A
// test that fails while the loop is parked releases it before Close.
func (o *observedWatcher) hold(t *testing.T) (release func()) {
	t.Helper()
	o.next++
	sentinel := filepath.Join(o.watcher.root, fmt.Sprintf("%d.sentinel", o.next))
	resume := make(chan struct{})
	release = sync.OnceFunc(func() { close(resume) })
	t.Cleanup(release)
	o.holdMu.Lock()
	o.holdPath, o.resume = sentinel, resume
	o.holdMu.Unlock()
	if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	o.waitHandled(t, sentinel)
	return release
}

func (o *observedWatcher) writeSentinel(t *testing.T) string {
	t.Helper()
	o.next++
	sentinel := filepath.Join(o.watcher.root, fmt.Sprintf("%d.sentinel", o.next))
	if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return sentinel
}

func (o *observedWatcher) waitHandled(t *testing.T, sentinel string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case path := <-o.handled:
			if path == sentinel {
				return
			}
		case <-deadline:
			t.Fatalf("the watch loop never handled sentinel %s", sentinel)
		}
	}
}

func (o *observedWatcher) expectNoEmit(t *testing.T, why string) {
	t.Helper()
	select {
	case <-o.emitted:
		t.Fatalf("%s emitted a definitions-changed event", why)
	default:
	}
}

// expectQueued syncs and asserts that the changes since armedBefore queued
// at least minimum debounce waits.
func (o *observedWatcher) expectQueued(t *testing.T, armedBefore, minimum int, why string) {
	t.Helper()
	o.sync(t)
	if armed := o.clock.armedCount() - armedBefore; armed < minimum {
		t.Fatalf("%s queued %d debounce waits, want at least %d", why, armed, minimum)
	}
	o.expectNoEmit(t, why+" before its debounce elapsed")
}

// expectIgnored syncs and asserts that the changes since armedBefore queued
// nothing.
func (o *observedWatcher) expectIgnored(t *testing.T, armedBefore int, why string) {
	t.Helper()
	o.sync(t)
	if armed := o.clock.armedCount() - armedBefore; armed != 0 {
		t.Fatalf("%s queued %d debounce waits, want none", why, armed)
	}
	o.expectNoEmit(t, why)
}

// releaseOneEmit lets the pending debounce waits elapse oldest first and
// asserts that only the wait queued by the last event emits, exactly once.
func (o *observedWatcher) releaseOneEmit(t *testing.T, why string) {
	t.Helper()
	if o.clock.fireAbandoned() > 0 {
		o.sync(t)
		o.expectNoEmit(t, why+" through a replaced debounce wait")
	}
	if !o.clock.fireLatest() {
		t.Fatalf("%s: no debounce wait is pending", why)
	}
	o.waitEmit(t, why)
	o.sync(t)
	o.expectNoEmit(t, why+" a second time")
}

func (o *observedWatcher) waitEmit(t *testing.T, why string) {
	t.Helper()
	select {
	case <-o.emitted:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: timed out waiting for a definitions-changed event", why)
	}
}
