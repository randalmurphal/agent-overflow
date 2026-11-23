package fswatch_test

import (
	"bytes"
	"errors"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/fswatch"
	"agent-overflow/internal/fswatch/fswatchtest"
)

// The test handler's rule: an event whose name ends in changeSuffix is a
// change, any other is not, and the event named holdName parks the handler
// until the test releases it.
const (
	changeSuffix = ".change"
	holdName     = "hold"
)

// driven is a Loop fed from channels the test owns. The channels are
// unbuffered, so a send returns once the loop has taken the event, which is
// after it finished every event before it.
type driven struct {
	loop    *fswatch.Loop
	clock   *fswatchtest.Clock
	events  chan fsnotify.Event
	errors  chan error
	emitted atomic.Int32
	emits   chan struct{}
	closed  atomic.Int32

	mu      sync.Mutex
	handled []string
	// held is told when the handler parks on holdName and again when it
	// sees the stop signal; release lets it return.
	held    chan string
	release chan struct{}
}

func startDriven(t *testing.T, debounce time.Duration, clock *fswatchtest.Clock) *driven {
	t.Helper()
	d := &driven{
		clock: clock, events: make(chan fsnotify.Event), errors: make(chan error),
		emits: make(chan struct{}, 8), held: make(chan string, 2), release: make(chan struct{}),
	}
	config := fswatch.Config{Label: "test watcher", Debounce: debounce, Emit: func() {
		d.emitted.Add(1)
		d.emits <- struct{}{}
	}}
	if clock != nil {
		config.After = clock.After
	}
	source := fswatch.Source{Events: d.events, Errors: d.errors, Close: func() error {
		d.closed.Add(1)
		return nil
	}}
	loop, err := fswatch.Start(config, source, d.handle)
	if err != nil {
		t.Fatal(err)
	}
	d.loop = loop
	t.Cleanup(func() {
		d.releaseHold()
		if err := closeWithin(t, loop); err != nil {
			t.Errorf("close loop: %v", err)
		}
	})
	return d
}

func (d *driven) handle(event fsnotify.Event, stop <-chan struct{}) bool {
	d.mu.Lock()
	d.handled = append(d.handled, event.Name)
	d.mu.Unlock()
	if event.Name == holdName {
		d.held <- "parked"
		select {
		case <-stop:
			d.held <- "stopping"
			<-d.release
		case <-d.release:
		}
		return false
	}
	return strings.HasSuffix(event.Name, changeSuffix)
}

func (d *driven) releaseHold() {
	select {
	case <-d.release:
	default:
		close(d.release)
	}
}

// send hands the loop one event and returns once the loop has taken it.
func (d *driven) send(t *testing.T, name string, op fsnotify.Op) {
	t.Helper()
	select {
	case d.events <- fsnotify.Event{Name: name, Op: op}:
	case <-time.After(5 * time.Second):
		t.Fatalf("the loop never took %s", name)
	}
}

// settle sends one event that is no change, so every event sent before it
// has been handled in full, including any wait it started.
func (d *driven) settle(t *testing.T) { d.send(t, "settle", fsnotify.Write) }

func (d *driven) waitEmit(t *testing.T, why string) {
	t.Helper()
	select {
	case <-d.emits:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: timed out waiting for an emit", why)
	}
}

func TestLoopDebouncesABurstToOneEmit(t *testing.T) {
	clock := &fswatchtest.Clock{}
	d := startDriven(t, 25*time.Millisecond, clock)

	d.send(t, "a"+changeSuffix, fsnotify.Create)
	d.send(t, "a"+changeSuffix, fsnotify.Write)
	d.send(t, "noise", fsnotify.Write)
	d.settle(t)
	if armed := clock.Armed(); armed != 2 {
		t.Fatalf("two changes and two non-changes started %d debounce waits, want 2", armed)
	}
	for _, duration := range clock.Durations() {
		if duration != 25*time.Millisecond {
			t.Fatalf("debounce wait = %v, want the configured 25ms", duration)
		}
	}
	if n := d.emitted.Load(); n != 0 {
		t.Fatalf("emitted %d times before any wait lapsed", n)
	}

	// The latest wait lapses first, so the emit is that wait's.
	clock.Fire(t, 1)
	d.waitEmit(t, "the burst's last wait")
	// The replaced wait lapses after. A loop still waiting on it would
	// take it at once; Close joins the loop, so its emit is counted.
	clock.Fire(t, 0)
	d.settle(t)
	if err := closeWithin(t, d.loop); err != nil {
		t.Fatal(err)
	}
	if n := d.emitted.Load(); n != 1 {
		t.Fatalf("a burst emitted %d times, want once", n)
	}
}

func TestLoopNeverHandsAPermissionOnlyEvent(t *testing.T) {
	d := startDriven(t, time.Hour, &fswatchtest.Clock{})
	d.send(t, "a"+changeSuffix, fsnotify.Chmod)
	d.settle(t)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range d.handled {
		if name == "a"+changeSuffix {
			t.Fatal("a permission-only event reached the handler")
		}
	}
}

// TestLoopDebouncesOnTheRealClock runs the wait production runs: a loop
// started without After waits on time.After, which the held clock stands
// in for everywhere else. The emit may not come before the debounce has
// passed.
func TestLoopDebouncesOnTheRealClock(t *testing.T) {
	const debounce = 20 * time.Millisecond
	d := startDriven(t, debounce, nil)
	sent := time.Now()
	d.send(t, "a"+changeSuffix, fsnotify.Write)
	d.waitEmit(t, "a change on the real clock")
	if elapsed := time.Since(sent); elapsed < debounce {
		t.Fatalf("emitted %v after the change, before the %v debounce", elapsed, debounce)
	}
}

// TestLoopCloseDropsAPendingWait closes the loop while a debounce wait is
// pending. Nothing may emit, Close closes the source and leaves none of
// the loop's goroutines behind, and a second Close is a no-op.
func TestLoopCloseDropsAPendingWait(t *testing.T) {
	clock := &fswatchtest.Clock{}
	d := startDriven(t, time.Hour, clock)
	d.send(t, "a"+changeSuffix, fsnotify.Write)
	d.settle(t)
	if clock.Armed() != 1 {
		t.Fatal("the change started no debounce wait")
	}
	if len(loopGoroutines()) == 0 {
		t.Fatal("no loop goroutine found while the loop is open")
	}
	if err := closeWithin(t, d.loop); err != nil {
		t.Fatal(err)
	}
	// The loop has exited, so the wait lapsing now reaches nobody.
	clock.Fire(t, 0)
	if n := d.emitted.Load(); n != 0 {
		t.Fatalf("closing with a pending debounce wait emitted %d times", n)
	}
	if left := awaitNoLoopGoroutines(5 * time.Second); len(left) != 0 {
		t.Fatalf("Close left %d loop goroutines:\n%s", len(left), strings.Join(left, "\n\n"))
	}
	if err := closeWithin(t, d.loop); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if n := d.closed.Load(); n != 1 {
		t.Fatalf("the source was closed %d times, want once", n)
	}
}

// TestLoopCloseWaitsForTheLoop parks the loop in the handler and checks
// that Close is still waiting for it, by finding Close blocked on the loop's
// exit rather than by waiting for some time to pass.
func TestLoopCloseWaitsForTheLoop(t *testing.T) {
	d := startDriven(t, time.Hour, &fswatchtest.Clock{})
	go func() { d.events <- fsnotify.Event{Name: holdName, Op: fsnotify.Write} }()
	<-d.held
	closed := make(chan error, 1)
	go func() { closed <- d.loop.Close() }()
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
	d.releaseHold()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the loop was released")
	}
}

// TestLoopEmitsNothingOnceCloseHasBegun lets a wait lapse while the handler
// holds the loop, begins Close, and only then releases the handler: the
// loop finds the lapsed wait and the stop both ready. Go picks either case
// of a select at random, so the sequence runs 64 times; a loop that emits
// for the lapsed wait passes all of them about once in 1.8e19.
func TestLoopEmitsNothingOnceCloseHasBegun(t *testing.T) {
	for range 64 {
		clock := &fswatchtest.Clock{}
		d := startDriven(t, time.Hour, clock)
		d.send(t, "a"+changeSuffix, fsnotify.Write)
		go func() { d.events <- fsnotify.Event{Name: holdName, Op: fsnotify.Write} }()
		if got := <-d.held; got != "parked" {
			t.Fatalf("handler = %s, want parked", got)
		}
		clock.Fire(t, 0)
		closed := make(chan error, 1)
		go func() { closed <- d.loop.Close() }()
		if got := <-d.held; got != "stopping" {
			t.Fatalf("handler = %s, want stopping", got)
		}
		d.releaseHold()
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close never returned")
		}
		if n := d.emitted.Load(); n != 0 {
			t.Fatalf("emitted %d times after Close began", n)
		}
	}
}

func TestStartRefusesAnInvalidConfigOrAMissingPartAndClosesTheSource(t *testing.T) {
	valid := fswatch.Config{Label: "test watcher", Debounce: time.Second, Emit: func() {}}
	handle := func(fsnotify.Event, <-chan struct{}) bool { return true }
	cases := []struct {
		name   string
		config func(fswatch.Config) fswatch.Config
		events bool
		handle fswatch.Handler
		want   string
	}{
		{"zero debounce", func(c fswatch.Config) fswatch.Config { c.Debounce = 0; return c }, true, handle, "test watcher: debounce must be positive"},
		{"negative debounce", func(c fswatch.Config) fswatch.Config { c.Debounce = -time.Second; return c }, true, handle, "test watcher: debounce must be positive"},
		{"nil emit", func(c fswatch.Config) fswatch.Config { c.Emit = nil; return c }, true, handle, "test watcher: emit callback is required"},
		{"no events", func(c fswatch.Config) fswatch.Config { return c }, false, handle, "test watcher: an event source is required"},
		{"nil handler", func(c fswatch.Config) fswatch.Config { return c }, true, nil, "test watcher: an event handler is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			closed := 0
			source := fswatch.Source{Errors: make(chan error), Close: func() error { closed++; return nil }}
			if tc.events {
				source.Events = make(chan fsnotify.Event)
			}
			if _, err := fswatch.Start(tc.config(valid), source, tc.handle); err == nil || err.Error() != tc.want {
				t.Fatalf("Start error = %v, want %q", err, tc.want)
			}
			if closed != 1 {
				t.Fatalf("a refused Start closed its source %d times, want once", closed)
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
	d := startDriven(t, time.Hour, clock)
	// errors is unbuffered: the send returns once the loop has taken it,
	// and the loop logs before it takes the next event.
	d.errors <- errors.New("queue overflow")
	d.send(t, "a"+changeSuffix, fsnotify.Write)
	d.settle(t)
	if got := logged.String(); got != "test watcher: queue overflow\n" {
		t.Fatalf("logged %q, want the watcher's label and the error", got)
	}
	if clock.Armed() != 1 {
		t.Fatal("the loop stopped handling changes after a watcher error")
	}
}

// closeWithin closes loop and fails the test if Close has not returned
// after five seconds, which only a Close that never joins reaches. The
// abandoned Close goroutine is left to the failed test.
func closeWithin(t *testing.T, loop *fswatch.Loop) error {
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
// package's loop. Matching stacks rather than counting goroutines keeps
// runtime and test framework goroutines out of the result.
func loopGoroutines() []string {
	var found []string
	for _, stack := range goroutineStacks() {
		if strings.Contains(stack, "fswatch.(*Loop).run") {
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
