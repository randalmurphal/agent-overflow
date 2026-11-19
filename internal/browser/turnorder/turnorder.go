// Package turnorder admits one pane mount's numbered commands in the order
// the user made them. Each command runs on its own goroutine, so two made in
// a row can arrive in either order; the client numbers them when the user
// acts, consecutive from 1 in each mount, and an Order gives each its Turn in
// that order:
//
//   - The next number takes its turn at once. The command ends it with
//     Commit in the hold that acts (the Manager's lock hold that picks or
//     records the tab), and the count advances. Work after the commit runs
//     outside the turn.
//   - A higher number waits for its predecessors' commits, never for their
//     work after it. When no turn has been taken for one gap wait the count
//     moves to the lowest number waiting. The numbers it moved past lose
//     their place, not their effect: they are recorded (MaxSkipped per
//     Order), and one that arrives later is admitted at once. A skipped
//     number the record no longer holds is refused with ErrLost when it
//     arrives; so is a repeat of an admitted number at or below the highest
//     one dropped, which the record cannot tell from it.
//   - A turn admitted after the count moved past missing numbers while it
//     waited, or a skipped number admitted late, stands OutOfOrder: a
//     command that names its target acts anyway, and one whose target
//     depends on its predecessors (the active tab) refuses.
//   - A repeat of a number admitted or waiting is Passed: it acts on
//     nothing. End (the mount detached) releases the waiters as Passed, and
//     a turn admitted before it commits Passed.
//   - Past MaxWaiters waiting, a number is refused with ErrTooManyWaiting
//     and becomes a gap: sent again before the count passes it, it waits its
//     turn; after, it is a skipped number.
//
// The order holds across late numbers, not across numbers never sent.
package turnorder

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Seq is a command's number in its mount, consecutive from 1.
type Seq uint64

const (
	// GapWait is how long waiting numbers wait, with no turn taken, for a
	// predecessor before it loses its place.
	GapWait = time.Second
	// MaxWaiters bounds the numbers waiting in one Order.
	MaxWaiters = 256
	// MaxSkipped bounds the numbers an Order remembers as skipped.
	MaxSkipped = 256
)

// ErrLost refuses a skipped number the Order no longer remembers.
var ErrLost = errors.New("browser: this tab action arrived too late to apply; try it again")

// ErrTooManyWaiting refuses a number past MaxWaiters.
var ErrTooManyWaiting = errors.New("browser: too many tab actions waiting in this pane")

// Standing is how a turn stands when it commits. The zero value is Passed,
// which acts on nothing.
type Standing int

const (
	// Passed acts on nothing: a repeat, or its mount is gone.
	Passed Standing = iota
	// InOrder follows every lower number's commit.
	InOrder
	// OutOfOrder was admitted past a missing lower number: after the count
	// moved past it while this one waited, or as a skipped number arriving
	// late.
	OutOfOrder
)

// GapTimer starts one gap wait and answers its expiry and its stop.
type GapTimer func() (<-chan time.Time, func() bool)

// NewGapTimer is the GapTimer of GapWait.
func NewGapTimer() (<-chan time.Time, func() bool) {
	timer := time.NewTimer(GapWait)
	return timer.C, timer.Stop
}

// Order is one mount's admission state.
type Order struct {
	gap GapTimer

	mu sync.Mutex
	// next is the number admitted next; taken is set while it holds its
	// turn, admitted and not yet ended.
	next  Seq
	taken bool
	// waiting holds the numbers above next waiting for their turn.
	waiting map[Seq]struct{}
	// skipped holds numbers below next the count moved past before they
	// arrived. lostThrough is the highest one dropped from it at the bound.
	skipped     map[Seq]struct{}
	lostThrough Seq
	// skips counts the moves of the count past missing numbers.
	skips uint64
	ended bool
	// changed is closed, and replaced, on every change a waiter waits for.
	changed chan struct{}
}

// New answers an empty Order whose gap waits gap starts.
func New(gap GapTimer) *Order {
	return &Order{
		gap:     gap,
		next:    1,
		waiting: make(map[Seq]struct{}),
		skipped: make(map[Seq]struct{}),
		changed: make(chan struct{}),
	}
}

// Turn is one admitted command's place. It belongs to the command's
// goroutine, which ends it once: with Commit in the hold that acts, or with
// Release when it acts on nothing.
type Turn struct {
	order    *Order
	seq      Seq
	standing Standing
	// done is guarded by order.mu for an ordered turn.
	done bool
}

// Unordered is the turn of a command with no number: it never waits and
// commits InOrder.
func Unordered() *Turn { return &Turn{standing: InOrder} }

// PassedTurn is the turn of a command whose mount the caller does not have:
// it commits Passed.
func PassedTurn() *Turn { return &Turn{done: true} }

// Await admits seq and answers once its turn has come: at once for a
// skipped number arriving late, or at once with a Passed turn. A number of
// 0 is refused. ctx ends a wait with its error.
func (o *Order) Await(ctx context.Context, seq Seq) (*Turn, error) {
	if seq == 0 {
		return nil, errors.New("browser: a tab action's number starts at 1")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return PassedTurn(), nil
	}
	if seq < o.next {
		if _, skipped := o.skipped[seq]; skipped {
			delete(o.skipped, seq)
			return &Turn{order: o, seq: seq, standing: OutOfOrder}, nil
		}
		if seq <= o.lostThrough {
			return nil, ErrLost
		}
		return PassedTurn(), nil
	}
	if _, repeat := o.waiting[seq]; repeat || (seq == o.next && o.taken) {
		return PassedTurn(), nil
	}
	if seq > o.next {
		if len(o.waiting) >= MaxWaiters {
			return nil, ErrTooManyWaiting
		}
		o.waiting[seq] = struct{}{}
	}
	skips := o.skips
	for seq != o.next || o.taken {
		changed := o.changed
		o.mu.Unlock()
		expired, stop := o.gap()
		var err error
		select {
		case <-changed:
		case <-expired:
		case <-ctx.Done():
			err = ctx.Err()
		}
		stop()
		o.mu.Lock()
		if err != nil || o.ended {
			delete(o.waiting, seq)
			if err != nil {
				return nil, err
			}
			return PassedTurn(), nil
		}
		select {
		case <-changed:
		default:
			// No turn was taken for the whole wait: the numbers missing
			// below the lowest one waiting lose their place.
			if low := o.lowestWaitingLocked(); !o.taken && low > o.next {
				o.skipToLocked(low)
			}
		}
	}
	delete(o.waiting, seq)
	o.taken = true
	standing := InOrder
	if o.skips != skips {
		standing = OutOfOrder
	}
	return &Turn{order: o, seq: seq, standing: standing}, nil
}

// End releases the waiters as Passed and makes every admitted turn commit
// Passed: the mount detached. The skipped record goes with it.
func (o *Order) End() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.ended {
		o.ended = true
		o.skipped = nil
		o.wakeLocked()
	}
}

// Stats is what an Order holds, for tests and diagnostics.
type Stats struct {
	Waiting, Skipped int
	Taken            bool
}

// Stats answers what o holds now.
func (o *Order) Stats() Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return Stats{Waiting: len(o.waiting), Skipped: len(o.skipped), Taken: o.taken}
}

// Commit ends t and answers how it stands: Passed when its mount ended
// after admitting it, or when t already ended, and the caller then acts on
// nothing.
func (t *Turn) Commit() Standing {
	if t.order == nil {
		if t.done {
			return Passed
		}
		t.done = true
		return t.standing
	}
	o := t.order
	o.mu.Lock()
	defer o.mu.Unlock()
	if t.done {
		return Passed
	}
	o.endLocked(t)
	if o.ended {
		return Passed
	}
	return t.standing
}

// Standing answers how t would stand if it committed now, without ending it.
func (t *Turn) Standing() Standing {
	if t.order == nil {
		if t.done {
			return Passed
		}
		return t.standing
	}
	t.order.mu.Lock()
	defer t.order.mu.Unlock()
	if t.done || t.order.ended {
		return Passed
	}
	return t.standing
}

// Release ends t if no Commit did, so a command that failed before acting
// still lets the next one in. Safe to call more than once.
func (t *Turn) Release() {
	if t.order == nil {
		t.done = true
		return
	}
	t.order.mu.Lock()
	defer t.order.mu.Unlock()
	if !t.done {
		t.order.endLocked(t)
	}
}

func (o *Order) endLocked(t *Turn) {
	t.done = true
	if o.taken && o.next == t.seq {
		o.taken = false
		o.next = t.seq + 1
		o.wakeLocked()
	}
}

func (o *Order) wakeLocked() {
	close(o.changed)
	o.changed = make(chan struct{})
}

// lowestWaitingLocked is the lowest number waiting, 0 for none.
func (o *Order) lowestWaitingLocked() Seq {
	var low Seq
	for seq := range o.waiting {
		if low == 0 || seq < low {
			low = seq
		}
	}
	return low
}

// skipToLocked moves the count to low, recording the numbers it passes.
// Past MaxSkipped the lowest are dropped into lostThrough.
func (o *Order) skipToLocked(low Seq) {
	from := o.next
	if low-from > MaxSkipped {
		from = low - MaxSkipped
		o.lostThrough = max(o.lostThrough, from-1)
	}
	for seq := from; seq < low; seq++ {
		if len(o.skipped) >= MaxSkipped {
			var lowest Seq
			for s := range o.skipped {
				if lowest == 0 || s < lowest {
					lowest = s
				}
			}
			delete(o.skipped, lowest)
			o.lostThrough = max(o.lostThrough, lowest)
		}
		o.skipped[seq] = struct{}{}
	}
	o.next = low
	o.skips++
	o.wakeLocked()
}
