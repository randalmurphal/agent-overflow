package accountswitch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// settledNow reports, without waiting, whether Settle called now would
// return at once.
func settledNow(c *Coordinator) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unsettledLocked(c.pending.seq) == ""
}

// endedSettle is Settle with a context that has already ended: what it says
// is unsettled now.
func endedSettle(c *Coordinator) error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return c.Settle(ctx)
}

// A Settle that cannot finish says what it waited for.
func TestSettleNamesWhatIsUnsettled(t *testing.T) {
	t.Run("worker not started", func(t *testing.T) {
		c := New(pastThresholdWhileRunning().deps())
		c.ReadingUpdated("claude", "b")
		if err := endedSettle(c); err == nil || !strings.Contains(err.Error(), "the worker has not started") {
			t.Fatalf("Settle = %v", err)
		}
	})
	t.Run("a pass still deciding", func(t *testing.T) {
		f := pastThresholdWhileRunning()
		g := newGate()
		f.beforeSwitch = func(string) { g.hold() }
		c := New(f.deps())
		startHeld(t, context.Background(), c, g)
		c.ReadingUpdated("claude", "b")
		<-g.entered
		err := endedSettle(c)
		if err == nil || !strings.Contains(err.Error(), "the worker is still deciding on the work queued before the wait") ||
			!errors.Is(err, context.Canceled) {
			t.Fatalf("Settle = %v", err)
		}
	})
	t.Run("a continue still running", func(t *testing.T) {
		f, c := readyToContinue()
		g := newGate()
		f.continueHook = func(context.Context, string) error {
			g.hold()
			return nil
		}
		startHeld(t, context.Background(), c, g)
		c.ReadingUpdated("claude", "a")
		<-g.entered
		awaitPassesDecided(c)
		if err := endedSettle(c); err == nil || !strings.Contains(err.Error(), "the continue of thread t1 is still running") {
			t.Fatalf("Settle = %v", err)
		}
	})
}

// awaitPassesDecided waits until the worker has finished deciding on every
// trigger queued so far, without regard to the continues it started.
func awaitPassesDecided(c *Coordinator) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for target := c.pending.seq; c.settled < target; {
		progress := c.progress
		c.mu.Unlock()
		<-progress
		c.mu.Lock()
	}
}

func settleAsync(ctx context.Context, c *Coordinator) <-chan error {
	out := make(chan error, 1)
	go func() { out <- c.Settle(ctx) }()
	return out
}

// gate holds the first caller of hold until release; entered closes when it
// starts waiting.
type gate struct {
	entered, released chan struct{}
	enter, open       sync.Once
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}), released: make(chan struct{})}
}

func (g *gate) hold() {
	first := false
	g.enter.Do(func() { first = true })
	if !first {
		return
	}
	close(g.entered)
	<-g.released
}

func (g *gate) release() { g.open.Do(func() { close(g.released) }) }

// startHeld runs the worker for the rest of the test. A failing test
// releases g before Stop joins the worker.
func startHeld(t *testing.T, ctx context.Context, c *Coordinator, g *gate) {
	t.Helper()
	c.Start(ctx)
	t.Cleanup(c.Stop)
	t.Cleanup(g.release)
}

// pastThresholdWhileRunning is b selected at 99% with a turn in flight and a
// clear, so a reading of b switches to a.
func pastThresholdWhileRunning() *fakeApp {
	f := newFakeApp("claude", "b", "a", "b")
	f.setReading("claude", "a", testNow, session(10, at(time.Hour)))
	f.setReading("claude", "b", testNow, session(99, at(time.Hour)))
	f.inflight["claude"] = []ThreadModel{{"t1", opusModel}}
	return f
}

// A reading is settled only once the pass that took it has decided, its
// switch included, and a trigger queued meanwhile is waited for too.
func TestSettleWaitsForThePassDecidingOnAReading(t *testing.T) {
	f := pastThresholdWhileRunning()
	g := newGate()
	f.beforeSwitch = func(string) { g.hold() }
	c := New(f.deps())
	startHeld(t, context.Background(), c, g)

	c.ReadingUpdated("claude", "b")
	<-g.entered
	if settledNow(c) {
		t.Fatal("settled while the worker was still deciding on the reading")
	}
	c.ReadingUpdated("claude", "b")
	settled := settleAsync(context.Background(), c)
	g.release()
	if err := <-settled; err != nil {
		t.Fatalf("Settle = %v", err)
	}
	if !settledNow(c) {
		t.Fatal("Settle returned with queued work undecided")
	}
	if switches, reports, _, _ := f.snapshot(); !equal(switches, []string{"a"}) || len(reports) != 1 {
		t.Fatalf("switches = %v reports = %+v, want one switch to a", switches, reports)
	}
}

// A continue a pass started is part of that pass's outcome: Settle waits for
// it to return.
func TestSettleWaitsForTheContinuesItsPassesStarted(t *testing.T) {
	f, c := readyToContinue()
	g := newGate()
	f.continueHook = func(context.Context, string) error {
		g.hold()
		return nil
	}
	startHeld(t, context.Background(), c, g)

	c.ReadingUpdated("claude", "a")
	<-g.entered
	awaitPassesDecided(c)
	if settledNow(c) {
		t.Fatal("settled while a continue the pass started was still running")
	}
	settled := settleAsync(context.Background(), c)
	g.release()
	if err := <-settled; err != nil {
		t.Fatalf("Settle = %v", err)
	}
	if _, _, _, cont := f.snapshot(); !equal(cont, []string{"t1"}) {
		t.Fatalf("continued = %v, want t1 by the time Settle returned", cont)
	}
}

// Triggers queued before Start are settled once the worker decides on them.
func TestSettleBeforeStartWaitsForTheWorker(t *testing.T) {
	f := pastThresholdWhileRunning()
	c := New(f.deps())
	c.ReadingUpdated("claude", "b")
	settled := settleAsync(context.Background(), c)
	if settledNow(c) {
		t.Fatal("settled before the worker ran")
	}
	startHeld(t, context.Background(), c, newGate())
	if err := <-settled; err != nil {
		t.Fatalf("Settle = %v", err)
	}
	if switches, _, _, _ := f.snapshot(); !equal(switches, []string{"a"}) {
		t.Fatalf("switches = %v, want the decision made before Settle returned", switches)
	}
}

// A worker stopped inside the pass deciding on a trigger never settles it,
// and Settle says so instead of waiting.
func TestSettleReportsAStopBeforeTheDecision(t *testing.T) {
	f := pastThresholdWhileRunning()
	g := newGate()
	f.beforeSwitch = func(string) { g.hold() }
	c := New(f.deps())
	ctx, cancel := context.WithCancel(context.Background())
	startHeld(t, ctx, c, g)

	c.ReadingUpdated("claude", "b")
	<-g.entered
	settled := settleAsync(context.Background(), c)
	cancel()
	g.release()
	err := <-settled
	if err == nil || !strings.Contains(err.Error(), "stopped before deciding") {
		t.Fatalf("Settle = %v, want the stop reported", err)
	}

	ended, end := context.WithCancel(context.Background())
	end()
	if err := c.Settle(ended); err == nil {
		t.Fatal("Settle with an ended context returned nil for undecided work")
	}
}
