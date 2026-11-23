package fswatch

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/fswatch/fswatchtest"
)

// File names the test handler treats as no change: the sync sentinels, and
// files a test writes to prove a non-change starts no wait.
const (
	sentinelExtension = ".sentinel"
	ignoredExtension  = ".ignored"
)

// observedLoop runs a Loop over one temp directory with a handler that
// reports each event it is handed, so a test can tell when the loop has
// handled every event it caused.
type observedLoop struct {
	dir     string
	loop    *Loop
	clock   *fswatchtest.Clock
	emitted chan struct{}
	handled chan string
	next    int

	holdMu   sync.Mutex
	holdName string
	resume   chan struct{}
}

// startObservedLoop starts a loop over a fresh directory. With a clock its
// waits are held; without one they are the production waits.
func startObservedLoop(t *testing.T, debounce time.Duration, clock *fswatchtest.Clock) *observedLoop {
	t.Helper()
	observed := &observedLoop{
		dir:     t.TempDir(),
		clock:   clock,
		emitted: make(chan struct{}, 8),
		handled: make(chan string, 64),
	}
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := fsWatcher.Add(observed.dir); err != nil {
		t.Fatal(errors.Join(err, fsWatcher.Close()))
	}
	config := Config{
		Label:    "test watcher",
		Watcher:  fsWatcher,
		Debounce: debounce,
		Handle:   observed.handle,
		Emit:     func() { observed.emitted <- struct{}{} },
	}
	if clock != nil {
		config.After = clock.After
	}
	loop, err := Start(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeWithin(t, loop); err != nil {
			t.Errorf("close loop: %v", err)
		}
	})
	observed.loop = loop
	return observed
}

func (o *observedLoop) handle(event fsnotify.Event, _ <-chan struct{}) bool {
	name := filepath.Base(event.Name)
	// Never block the loop on the report: Close waits for it. A dropped
	// name fails a waiting sync with a timeout naming its sentinel.
	select {
	case o.handled <- name:
	default:
	}
	o.holdMu.Lock()
	resume := o.resume
	parked := name == o.holdName
	o.holdMu.Unlock()
	if parked {
		<-resume
	}
	return !strings.HasSuffix(name, sentinelExtension) && !strings.HasSuffix(name, ignoredExtension)
}

func (o *observedLoop) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(o.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (o *observedLoop) writeSentinel(t *testing.T) string {
	t.Helper()
	o.next++
	name := fmt.Sprintf("%d%s", o.next, sentinelExtension)
	o.write(t, name, "")
	return name
}

// sync writes a sentinel and waits until the loop has handled it. The
// kernel queues a change's events before the syscall returns, and the loop
// handles them in delivery order on one goroutine, so every event from an
// earlier change has been handled too.
func (o *observedLoop) sync(t *testing.T) []string {
	t.Helper()
	return o.waitHandled(t, o.writeSentinel(t))
}

// waitHandled waits until the loop has handled sentinel and returns the
// names it handled before it.
func (o *observedLoop) waitHandled(t *testing.T, sentinel string) []string {
	t.Helper()
	var before []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case name := <-o.handled:
			if name == sentinel {
				return before
			}
			before = append(before, name)
		case <-deadline:
			t.Fatalf("the watch loop never handled sentinel %s", sentinel)
		}
	}
}

// hold parks the loop inside the handler on a sentinel until the returned
// release is called, so changes made meanwhile are all queued before the
// loop handles any. The parked handler ignores the stop signal, so Close
// cannot end the loop while it is held.
func (o *observedLoop) hold(t *testing.T) (release func()) {
	t.Helper()
	o.next++
	name := fmt.Sprintf("%d%s", o.next, sentinelExtension)
	resume := make(chan struct{})
	release = sync.OnceFunc(func() { close(resume) })
	t.Cleanup(release)
	o.holdMu.Lock()
	o.holdName, o.resume = name, resume
	o.holdMu.Unlock()
	o.write(t, name, "")
	o.waitHandled(t, name)
	return release
}

func (o *observedLoop) expectNoEmit(t *testing.T, why string) {
	t.Helper()
	select {
	case <-o.emitted:
		t.Fatalf("%s emitted", why)
	default:
	}
}

func (o *observedLoop) waitEmit(t *testing.T, why string) {
	t.Helper()
	select {
	case <-o.emitted:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: timed out waiting for an emit", why)
	}
}

func TestLoopDebouncesABurstToOneEmit(t *testing.T) {
	clock := &fswatchtest.Clock{}
	observed := startObservedLoop(t, 25*time.Millisecond, clock)

	// The first write alone is a CREATE then a WRITE, which inotify never
	// merges, so the burst starts the debounce more than once. kqueue finds
	// a new file by listing its directory and watches it only then, so a
	// burst that ends first can arrive as one event.
	burstWaits := 2
	if runtime.GOOS == "darwin" {
		burstWaits = 1
	}
	for index := range 3 {
		observed.write(t, "content.txt", string(rune('a'+index)))
	}
	observed.sync(t)
	if armed := clock.Armed(); armed < burstWaits {
		t.Fatalf("a write burst started %d debounce waits, want at least %d", armed, burstWaits)
	}
	for _, duration := range clock.Durations() {
		if duration != 25*time.Millisecond {
			t.Fatalf("debounce wait = %v, want the configured 25ms", duration)
		}
	}
	observed.expectNoEmit(t, "a write burst before its debounce lapsed")

	// The waits a later change replaced lapse first: none of them emits.
	if clock.FireAbandoned() > 0 {
		observed.sync(t)
		observed.expectNoEmit(t, "a replaced debounce wait")
	}
	if !clock.FireLatest() {
		t.Fatal("no debounce wait is pending")
	}
	observed.waitEmit(t, "the burst's last wait")
	observed.sync(t)
	observed.expectNoEmit(t, "a write burst a second time")

	// An event the handler reports as no change starts no wait.
	armed := clock.Armed()
	observed.write(t, "noise"+ignoredExtension, "x")
	observed.sync(t)
	if got := clock.Armed() - armed; got != 0 {
		t.Fatalf("an event that is no change started %d debounce waits", got)
	}
}

func TestLoopNeverHandsAPermissionOnlyEvent(t *testing.T) {
	observed := startObservedLoop(t, time.Hour, &fswatchtest.Clock{})
	path := filepath.Join(observed.dir, "content.txt")
	observed.write(t, "content.txt", "x")
	observed.sync(t)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if handled := observed.sync(t); len(handled) != 0 {
		t.Fatalf("a chmod reached the handler as %v", handled)
	}
}

// TestLoopDebouncesOnTheRealClock runs the wait production runs: a loop
// started without After waits on time.After. The held clock stands in for
// that wait everywhere else, so only this test proves the default. Each
// burst is written while the loop is parked, so the whole burst is queued
// before the loop handles any of it and lands inside one window.
func TestLoopDebouncesOnTheRealClock(t *testing.T) {
	observed := startObservedLoop(t, time.Second, nil)
	for burst := range 2 {
		why := fmt.Sprintf("write burst %d", burst+1)
		release := observed.hold(t)
		for index := range 3 {
			observed.write(t, "content.txt", string(rune('a'+index)))
		}
		release()
		observed.waitEmit(t, why)
		observed.sync(t)
		observed.expectNoEmit(t, why+" a second time")
	}
}

// TestLoopCloseDropsAPendingWait closes the loop while a debounce wait is
// pending. Nothing may emit, Close must leave none of the loop's
// goroutines behind, and a second Close is a no-op.
func TestLoopCloseDropsAPendingWait(t *testing.T) {
	clock := &fswatchtest.Clock{}
	observed := startObservedLoop(t, time.Hour, clock)
	observed.write(t, "content.txt", "x")
	observed.sync(t)
	if clock.Armed() == 0 {
		t.Fatal("the write started no debounce wait")
	}
	if len(loopGoroutines()) == 0 {
		t.Fatal("no loop goroutine found while the loop is open")
	}
	if err := closeWithin(t, observed.loop); err != nil {
		t.Fatal(err)
	}
	// The loop has exited, so the wait lapsing now reaches nobody.
	clock.FireLatest()
	observed.expectNoEmit(t, "closing with a pending debounce wait")
	if left := awaitNoLoopGoroutines(5 * time.Second); len(left) != 0 {
		t.Fatalf("Close left %d loop goroutines:\n%s", len(left), strings.Join(left, "\n\n"))
	}
	if err := closeWithin(t, observed.loop); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestLoopCloseWaitsForTheLoop parks the loop in the handler and checks
// that Close is still waiting for it, by finding Close blocked on the loop's
// exit rather than by waiting for some time to pass.
func TestLoopCloseWaitsForTheLoop(t *testing.T) {
	observed := startObservedLoop(t, time.Hour, &fswatchtest.Clock{})
	release := observed.hold(t)
	closed := make(chan error, 1)
	go func() { closed <- observed.loop.Close() }()
	deadline := time.After(5 * time.Second)
	for !closeIsWaitingForTheLoop() {
		select {
		case err := <-closed:
			t.Fatalf("Close returned (%v) while the loop was parked in the handler", err)
		case <-deadline:
			t.Fatal("Close never reached its wait for the loop")
		case <-time.After(time.Millisecond):
		}
	}
	release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the loop was released")
	}
}

func TestStartRejectsAnInvalidDebounceOrEmit(t *testing.T) {
	cases := []struct {
		name     string
		debounce time.Duration
		emit     func()
		want     string
	}{
		{"zero debounce", 0, func() {}, "test watcher: debounce must be positive"},
		{"negative debounce", -time.Second, func() {}, "test watcher: debounce must be positive"},
		{"nil emit", time.Second, nil, "test watcher: emit callback is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsWatcher, err := fsnotify.NewWatcher()
			if err != nil {
				t.Fatal(err)
			}
			_, err = Start(Config{
				Label: "test watcher", Watcher: fsWatcher, Debounce: tc.debounce,
				Handle: func(fsnotify.Event, <-chan struct{}) bool { return true }, Emit: tc.emit,
			})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Start error = %v, want %q", err, tc.want)
			}
			// Start owns the watcher it was handed, including on failure.
			if err := fsWatcher.Add(t.TempDir()); !errors.Is(err, fsnotify.ErrClosed) {
				t.Fatalf("Add after a failed Start = %v, want the watcher closed", err)
			}
		})
	}
}

func TestLoopLogsAWatcherErrorWithItsLabelAndKeepsRunning(t *testing.T) {
	var logged bytes.Buffer
	flags, prefix := log.Flags(), log.Prefix()
	log.SetOutput(&logged)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	clock := &fswatchtest.Clock{}
	observed := startObservedLoop(t, time.Hour, clock)
	fsWatcher := observed.loop.config.Watcher
	// Errors is unbuffered: the send returns once the loop has received it.
	fsWatcher.Errors <- errors.New("queue overflow")
	observed.write(t, "content.txt", "x")
	observed.sync(t)
	if got := logged.String(); got != "test watcher: queue overflow\n" {
		t.Fatalf("logged %q, want the watcher's label and the error", got)
	}
	if clock.Armed() == 0 {
		t.Fatal("the loop stopped handling changes after a watcher error")
	}
}

// closeWithin closes loop and fails the test if Close has not returned
// after five seconds, which only a Close that never joins reaches. The
// abandoned Close goroutine is left to the failed test.
func closeWithin(t *testing.T, loop *Loop) error {
	t.Helper()
	closed := make(chan error, 1)
	go func() { closed <- loop.Close() }()
	select {
	case err := <-closed:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned")
		return nil
	}
}

// loopGoroutines returns the stacks of live goroutines that run this
// package's loop or were started by fsnotify. Matching stacks rather than
// counting goroutines keeps runtime and test framework goroutines out of
// the result.
func loopGoroutines() []string {
	var found []string
	for _, stack := range goroutineStacks() {
		if strings.Contains(stack, "fswatch.(*Loop).run") || strings.Contains(stack, "github.com/fsnotify/fsnotify") {
			found = append(found, stack)
		}
	}
	return found
}

// awaitNoLoopGoroutines retries until no loop goroutine is left or the
// deadline passes, and returns what is left. Close joins the loop on a
// signal sent from its deferred call, so a joined goroutine can still be
// finishing when Close returns.
func awaitNoLoopGoroutines(within time.Duration) []string {
	deadline := time.Now().Add(within)
	for {
		left := loopGoroutines()
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func closeIsWaitingForTheLoop() bool {
	for _, stack := range goroutineStacks() {
		if strings.Contains(stack, "fswatch.(*Loop).Close") && strings.Contains(stack, "sync.(*WaitGroup).Wait") {
			return true
		}
	}
	return false
}

func goroutineStacks() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Split(string(buf[:n]), "\n\n")
		}
		buf = make([]byte, 2*len(buf))
	}
}
