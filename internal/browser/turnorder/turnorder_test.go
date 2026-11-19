package turnorder

import (
	"context"
	"errors"
	"fmt"
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

// at is the seq-th number of epoch 1.
func at(seq uint64) Place { return in(1, seq) }

// in is the seq-th number of epoch.
func in(epoch, seq uint64) Place {
	place, err := NewPlace(epoch, seq)
	if err != nil {
		panic(err)
	}
	return place
}

func await(o *Order, place Place) <-chan admission {
	done := make(chan admission, 1)
	go func() {
		turn, err := o.Await(context.Background(), place)
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

func mustAwait(t *testing.T, o *Order, place Place) *Turn {
	t.Helper()
	return admitted(t, await(o, place))
}

// refused fails unless done answers ErrLost.
func refused(t *testing.T, done <-chan admission) {
	t.Helper()
	select {
	case a := <-done:
		if !errors.Is(a.err, ErrLost) {
			t.Fatalf("answered %+v, %v; want ErrLost", a.turn, a.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never answered")
	}
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
	second := await(o, at(2))
	waitFor(t, o, 1)
	first := mustAwait(t, o, at(1))
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
	turn := mustAwait(t, o, at(1))
	if got := turn.Commit(); got != InOrder {
		t.Fatalf("first commit %d", got)
	}
	turn.Release()
	if got := turn.Commit(); got != Passed {
		t.Fatalf("second commit %d, want Passed", got)
	}
	next := mustAwait(t, o, at(2))
	next.Release()
	next.Release()
	if got := mustAwait(t, o, at(3)).Commit(); got != InOrder {
		t.Fatalf("3 after a released 2 stands %d", got)
	}
	unordered := Unordered()
	if unordered.Commit() != InOrder || unordered.Commit() != Passed {
		t.Fatal("an unordered turn did not end once")
	}
}

// The count moves past a missing number after one gap wait: the waiter
// stands OutOfOrder, and the missing number arriving late is refused, which
// settles it. A number arriving after that, in order, stands InOrder.
func TestAMissingNumberMakesTheTurnsAfterItOutOfOrderUntilItSettles(t *testing.T) {
	c := newClock()
	o := New(c.timer)
	second := await(o, at(2))
	c.expire(t)
	if got := admitted(t, second).Commit(); got != OutOfOrder {
		t.Fatalf("2 past a missing 1 stands %d, want OutOfOrder", got)
	}
	refused(t, await(o, at(1)))
	if got := o.Stats().Outstanding; got != 0 {
		t.Fatalf("%d outstanding after the late 1 was refused", got)
	}
	if got := mustAwait(t, o, at(3)).Commit(); got != InOrder {
		t.Fatalf("3 stands %d, want InOrder", got)
	}
}

// The review's sequence and its permutations, as the Manager sees select(B)
// #1, reload #2 and close #3: #1 is delayed, #2 refuses after the gap wait,
// and #3 arrives before #1, after the gap was recorded or waiting while it
// was. #3 stands OutOfOrder whoever saw the gap, #1 is refused when it
// arrives, and in a new epoch the next command stands InOrder.
func TestARelativeCommandAfterARecordedGapStaysOutOfOrder(t *testing.T) {
	for _, thirdWaits := range []bool{false, true} {
		t.Run(fmt.Sprintf("third waits %v", thirdWaits), func(t *testing.T) {
			c := newClock()
			o := New(c.timer)
			second := await(o, at(2))
			var third <-chan admission
			if thirdWaits {
				waitFor(t, o, 1)
				third = await(o, at(3))
				waitFor(t, o, 2)
			}
			c.expire(t)
			if got := admitted(t, second).Commit(); got != OutOfOrder {
				t.Fatalf("#2 stands %d, want OutOfOrder", got)
			}
			if third == nil {
				third = await(o, at(3))
			}
			if got := admitted(t, third).Commit(); got != OutOfOrder {
				t.Fatalf("#3 before the late #1 stands %d, want OutOfOrder", got)
			}
			refused(t, await(o, at(1)))
			if got := mustAwait(t, o, in(2, 1)).Commit(); got != InOrder {
				t.Fatalf("the new epoch's first stands %d, want InOrder", got)
			}
		})
	}
}

// A turn's standing is taken at its commit: admitted while an earlier
// number is outstanding it stands OutOfOrder, and once that number arrives
// and is refused it commits InOrder.
func TestStandingFollowsTheOutstandingNumbers(t *testing.T) {
	c := newClock()
	o := New(c.timer)
	second := await(o, at(2))
	c.expire(t)
	admitted(t, second).Commit()
	third := mustAwait(t, o, at(3))
	if got := third.Standing(); got != OutOfOrder {
		t.Fatalf("#3 with #1 outstanding stands %d", got)
	}
	refused(t, await(o, at(1)))
	if got := third.Commit(); got != InOrder {
		t.Fatalf("#3 commits %d once #1 settled, want InOrder", got)
	}
}

// A newer epoch begins the count again: a number of the older epoch is
// refused, the older epoch's waiters are refused, and its admitted turn
// commits Passed without moving the new count.
func TestANewerEpochRefusesTheOlderOne(t *testing.T) {
	o := New(newClock().timer)
	old := mustAwait(t, o, at(1))
	waiter := await(o, at(3))
	waitFor(t, o, 1)
	first := mustAwait(t, o, in(2, 1))
	refused(t, waiter)
	refused(t, await(o, at(2)))
	if got := old.Commit(); got != Passed {
		t.Fatalf("the older epoch's turn commits %d, want Passed", got)
	}
	if got := first.Commit(); got != InOrder {
		t.Fatalf("the new epoch's first commits %d, want InOrder", got)
	}
	if got := mustAwait(t, o, in(2, 2)).Commit(); got != InOrder {
		t.Fatalf("the new epoch's second commits %d, want InOrder", got)
	}
	if stats := o.Stats(); stats.Waiting != 0 || stats.Taken {
		t.Fatalf("the order holds %+v", stats)
	}
}

// A newer epoch drops an older one's outstanding numbers: a gap that never
// settles ends with its epoch.
func TestANewerEpochEndsAnUnsettledGap(t *testing.T) {
	c := newClock()
	o := New(c.timer)
	second := await(o, at(2))
	c.expire(t)
	admitted(t, second).Commit()
	if got := mustAwait(t, o, at(3)).Commit(); got != OutOfOrder {
		t.Fatalf("#3 stands %d with #1 never sent", got)
	}
	if got := mustAwait(t, o, in(2, 1)).Commit(); got != InOrder {
		t.Fatalf("the new epoch's first stands %d", got)
	}
	if got := o.Stats().Outstanding; got != 0 {
		t.Fatalf("the new epoch kept %d outstanding", got)
	}
}

// Repeats are Passed; a number dropped from the outstanding record is lost,
// and leaves its epoch out of order for good.
func TestRepeatsPassAndDroppedNumbersAreLost(t *testing.T) {
	c := newClock()
	o := New(c.timer)
	mustAwait(t, o, at(1)).Commit()
	if got := mustAwait(t, o, at(1)).Commit(); got != Passed {
		t.Fatalf("a repeat stands %d", got)
	}
	far := Seq(MaxSkipped + 10)
	waiter := await(o, at(uint64(far)))
	c.expire(t)
	admitted(t, waiter).Commit()
	if _, err := o.Await(context.Background(), at(5)); !errors.Is(err, ErrLost) {
		t.Fatalf("a dropped number answered %v", err)
	}
	if got := o.Stats().Outstanding; got != MaxSkipped {
		t.Fatalf("%d outstanding held, want %d", got, MaxSkipped)
	}
	for seq := far - MaxSkipped; seq < far; seq++ {
		refused(t, await(o, at(uint64(seq))))
	}
	if got := mustAwait(t, o, at(uint64(far+1))).Commit(); got != OutOfOrder {
		t.Fatalf("with a dropped number unsettled the next stands %d", got)
	}
	if _, err := o.Await(context.Background(), Place{}); err == nil {
		t.Fatal("number 0 was admitted")
	}
	if _, err := NewPlace(0, 1); err == nil {
		t.Fatal("epoch 0 was made")
	}
}

// End releases the waiters as Passed, makes an admitted turn commit Passed,
// and passes every later number.
func TestEndPassesEverything(t *testing.T) {
	o := New(newClock().timer)
	first := mustAwait(t, o, at(1))
	second := await(o, at(2))
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
	if got := mustAwait(t, o, at(3)).Commit(); got != Passed {
		t.Fatalf("a number after End stands %d", got)
	}
	if stats := o.Stats(); stats.Waiting != 0 || stats.Outstanding != 0 {
		t.Fatalf("an ended order holds %+v", stats)
	}
}

// Past MaxWaiters a number is refused, and a canceled wait leaves no
// waiter.
func TestWaitersAreBoundedAndCancelable(t *testing.T) {
	o := New(newClock().timer)
	for seq := Seq(2); seq < 2+MaxWaiters; seq++ {
		await(o, at(uint64(seq)))
	}
	waitFor(t, o, MaxWaiters)
	if _, err := o.Await(context.Background(), at(2+MaxWaiters)); !errors.Is(err, ErrTooManyWaiting) {
		t.Fatalf("a waiter past the bound answered %v", err)
	}
	o.End()
	waitFor(t, o, 0)

	o = New(newClock().timer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := o.Await(ctx, at(2))
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
