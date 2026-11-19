package turnorder

import (
	"context"
	"errors"
	"testing"
	"time"
)

// clock is a GapTimer whose waits expire only when the test fires them.
type clock struct {
	fire    chan time.Time
	started chan struct{}
}

func newClock() *clock {
	return &clock{fire: make(chan time.Time), started: make(chan struct{}, 64)}
}

func (c *clock) timer() (<-chan time.Time, func() bool) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	return c.fire, func() bool { return true }
}

// expire waits for a gap wait to start and ends it.
func (c *clock) expire(t *testing.T) {
	t.Helper()
	select {
	case <-c.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no gap wait started")
	}
	select {
	case c.fire <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("nothing waits on the gap timer")
	}
}

type admission struct {
	turn *Turn
	err  error
}

func await(o *Order, seq Seq) <-chan admission {
	done := make(chan admission, 1)
	go func() {
		turn, err := o.Await(context.Background(), seq)
		done <- admission{turn, err}
	}()
	return done
}

func admitted(t *testing.T, done <-chan admission) *Turn {
	t.Helper()
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatal(a.err)
		}
		return a.turn
	case <-time.After(5 * time.Second):
		t.Fatal("never admitted")
		return nil
	}
}

func mustAwait(t *testing.T, o *Order, seq Seq) *Turn {
	t.Helper()
	return admitted(t, await(o, seq))
}

func waitFor(t *testing.T, o *Order, waiting int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for o.Stats().Waiting != waiting {
		if time.Now().After(deadline) {
			t.Fatalf("%d waiting, want %d", o.Stats().Waiting, waiting)
		}
		time.Sleep(time.Millisecond)
	}
}

// Numbers arriving reversed take their turns in order, each standing in
// order, and a later one waits only for its predecessor's commit.
func TestNumbersTakeTheirTurnsInOrder(t *testing.T) {
	o := New(newClock().timer)
	second := await(o, 2)
	waitFor(t, o, 1)
	first := mustAwait(t, o, 1)
	select {
	case <-second:
		t.Fatal("2 was admitted before 1 committed")
	case <-time.After(20 * time.Millisecond):
	}
	if got := first.Commit(); got != InOrder {
		t.Fatalf("1 stands %d, want InOrder", got)
	}
	if got := admitted(t, second).Commit(); got != InOrder {
		t.Fatalf("2 stands %d, want InOrder", got)
	}
}

// A turn ends once: a second Commit, and a Release after it, change nothing
// and answer Passed.
func TestATurnEndsOnce(t *testing.T) {
	o := New(newClock().timer)
	turn := mustAwait(t, o, 1)
	if got := turn.Commit(); got != InOrder {
		t.Fatalf("first commit %d", got)
	}
	turn.Release()
	if got := turn.Commit(); got != Passed {
		t.Fatalf("second commit %d, want Passed", got)
	}
	next := mustAwait(t, o, 2)
	next.Release()
	next.Release()
	if got := mustAwait(t, o, 3).Commit(); got != InOrder {
		t.Fatalf("3 after a released 2 stands %d", got)
	}
	unordered := Unordered()
	if unordered.Commit() != InOrder || unordered.Commit() != Passed {
		t.Fatal("an unordered turn did not end once")
	}
}

// The count moves past a missing number after one gap wait: the waiter
// stands OutOfOrder, and the missing number arriving late is admitted at
// once, OutOfOrder too. A number arriving after that, in order, stands
// InOrder.
func TestAMissingNumberMakesTheTurnsAroundItOutOfOrder(t *testing.T) {
	c := newClock()
	o := New(c.timer)
	second := await(o, 2)
	c.expire(t)
	if got := admitted(t, second).Commit(); got != OutOfOrder {
		t.Fatalf("2 past a missing 1 stands %d, want OutOfOrder", got)
	}
	if got := mustAwait(t, o, 1).Commit(); got != OutOfOrder {
		t.Fatalf("a late 1 stands %d, want OutOfOrder", got)
	}
	if got := mustAwait(t, o, 3).Commit(); got != InOrder {
		t.Fatalf("3 stands %d, want InOrder", got)
	}
}

// Repeats are Passed; a dropped skipped number is lost.
func TestRepeatsPassAndDroppedNumbersAreLost(t *testing.T) {
	c := newClock()
	o := New(c.timer)
	mustAwait(t, o, 1).Commit()
	if got := mustAwait(t, o, 1).Commit(); got != Passed {
		t.Fatalf("a repeat stands %d", got)
	}
	far := Seq(MaxSkipped + 10)
	waiter := await(o, far)
	c.expire(t)
	admitted(t, waiter).Commit()
	if _, err := o.Await(context.Background(), 5); !errors.Is(err, ErrLost) {
		t.Fatalf("a dropped number answered %v", err)
	}
	if got := o.Stats().Skipped; got != MaxSkipped {
		t.Fatalf("%d skipped held, want %d", got, MaxSkipped)
	}
	if _, err := o.Await(context.Background(), 0); err == nil {
		t.Fatal("number 0 was admitted")
	}
}

// End releases the waiters as Passed, makes an admitted turn commit Passed,
// and passes every later number.
func TestEndPassesEverything(t *testing.T) {
	o := New(newClock().timer)
	first := mustAwait(t, o, 1)
	second := await(o, 2)
	waitFor(t, o, 1)
	o.End()
	if got := admitted(t, second).Commit(); got != Passed {
		t.Fatalf("a released waiter stands %d", got)
	}
	if got := first.Standing(); got != Passed {
		t.Fatalf("an admitted turn stands %d after End", got)
	}
	if got := first.Commit(); got != Passed {
		t.Fatalf("an admitted turn commits %d after End", got)
	}
	if got := mustAwait(t, o, 3).Commit(); got != Passed {
		t.Fatalf("a number after End stands %d", got)
	}
	if stats := o.Stats(); stats.Waiting != 0 || stats.Skipped != 0 {
		t.Fatalf("an ended order holds %+v", stats)
	}
}

// Past MaxWaiters a number is refused, and a canceled wait leaves no
// waiter.
func TestWaitersAreBoundedAndCancelable(t *testing.T) {
	o := New(newClock().timer)
	for seq := Seq(2); seq < 2+MaxWaiters; seq++ {
		await(o, seq)
	}
	waitFor(t, o, MaxWaiters)
	if _, err := o.Await(context.Background(), 2+MaxWaiters); !errors.Is(err, ErrTooManyWaiting) {
		t.Fatalf("a waiter past the bound answered %v", err)
	}
	o.End()
	waitFor(t, o, 0)

	o = New(newClock().timer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := o.Await(ctx, 2)
		done <- err
	}()
	waitFor(t, o, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled wait answered %v", err)
	}
	if got := o.Stats().Waiting; got != 0 {
		t.Fatalf("a canceled wait left %d waiting", got)
	}
}
