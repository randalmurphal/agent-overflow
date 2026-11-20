// Package turnorder admits one pane mount's numbered commands in the order
// the user made them. Each command runs on its own goroutine, so two made in
// a row can arrive in either order; the client numbers them when the user
// acts, and an Order gives each its Turn in that order. A number is a Place:
// an epoch and a sequence in it, consecutive from 1.
//
//   - The client starts a new epoch only when every command it sent in the
//     current one has answered or failed, so nothing of an older epoch is
//     still meant to act. The first number of a newer epoch starts it here:
//     the count begins again at 1, the older epoch's waiters are refused
//     with ErrLost, and its admitted turns commit Passed. A number of an
//     older epoch is refused with ErrLost.
//   - The next number takes its turn at once. The command ends it with
//     Commit in the hold that acts (the Manager's lock hold that picks or
//     records the tab), and the count advances. Work after the commit runs
//     outside the turn.
//   - A higher number waits for its predecessors' commits, never for their
//     work after it. When no turn has been taken for one gap wait the count
//     moves to the lowest number waiting. The numbers it moved past are
//     outstanding: each lost its place, and arriving later it is refused
//     with ErrLost, which settles it. The record of outstanding numbers is
//     bounded (MaxSkipped per epoch); a number dropped from it can never be
//     settled in its epoch.
//   - A turn admitted and not passed commits InOrder, outstanding lower
//     numbers or not: every command names its target or steps from what
//     has arrived, so none waits on a number that lost its place.
//   - A repeat of a number admitted or waiting is Passed: it acts on
//     nothing. End (the mount detached) releases the waiters as Passed, and
//     a turn admitted before it commits Passed.
//   - Past MaxWaiters waiting, a number is refused with ErrTooManyWaiting
//     and becomes a gap: sent again before the count passes it, it waits its
//     turn; after, it is outstanding and refused.
package turnorder

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Seq is a command's number in its epoch, consecutive from 1.
type Seq uint64

// Epoch is the client's run of commands the numbers count in, from 1.
type Epoch uint64

// Place is one command's number: its epoch and its sequence there.
type Place struct {
	epoch Epoch
	seq   Seq
}

// NewPlace answers the seq-th command of epoch; both start at 1.
func NewPlace(epoch, seq uint64) (Place, error) {
	if epoch == 0 || seq == 0 {
		return Place{}, errors.New("browser: a tab action's epoch and number start at 1")
	}
	return Place{epoch: Epoch(epoch), seq: Seq(seq)}, nil
}

const (
	// GapWait is how long waiting numbers wait, with no turn taken, for a
	// predecessor before it loses its place.
	GapWait = time.Second
	// MaxWaiters bounds the numbers waiting in one Order.
	MaxWaiters = 256
	// MaxSkipped bounds the outstanding numbers an Order remembers.
	MaxSkipped = 256
)

// ErrLost refuses a number that lost its place: the count moved past it
// before it arrived, or its epoch is over.
var ErrLost = errors.New("browser: this tab action arrived too late to apply; try it again")

// ErrTooManyWaiting refuses a number past MaxWaiters.
var ErrTooManyWaiting = errors.New("browser: too many tab actions waiting in this pane")

// Standing is how a turn stands when it commits. The zero value is Passed,
// which acts on nothing.
type Standing int

const (
	// Passed acts on nothing: a repeat, its mount is gone, or its epoch
	// ended after it was admitted.
	Passed Standing = iota
	// InOrder acts: every lower number of its epoch committed, was refused
	// or lost its place.
	InOrder
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
	// epoch is the newest epoch admitted; the fields below count in it.
	epoch Epoch
	// next is the number admitted next; taken is set while it holds its
	// turn, admitted and not yet ended.
	next  Seq
	taken bool
	// waiting holds the numbers above next waiting for their turn.
	waiting map[Seq]struct{}
	// outstanding holds numbers below next the count moved past before
	// they arrived. lostThrough is the highest one dropped from it at the
	// bound, which leaves the epoch outstanding for good.
	outstanding map[Seq]struct{}
	lostThrough Seq
	ended       bool
	// changed is closed, and replaced, on every change a waiter waits for.
	changed chan struct{}
}

// New answers an empty Order whose gap waits gap starts.
func New(gap GapTimer) *Order {
	return &Order{
		gap:         gap,
		next:        1,
		waiting:     make(map[Seq]struct{}),
		outstanding: make(map[Seq]struct{}),
		changed:     make(chan struct{}),
	}
}

// Turn is one admitted command's place. It belongs to the command's
// goroutine, which ends it once: with Commit in the hold that acts, or with
// Release when it acts on nothing.
type Turn struct {
	order *Order
	place Place
	// unordered marks the turn of a command with no number.
	unordered bool
	// done is guarded by order.mu for an ordered turn.
	done bool
}

// Unordered is the turn of a command with no number: it never waits and
// commits InOrder.
func Unordered() *Turn { return &Turn{unordered: true} }

// PassedTurn is the turn of a command whose mount the caller does not have:
// it commits Passed.
func PassedTurn() *Turn { return &Turn{done: true} }

// Await admits at and answers once its turn has come, or at once with a
// Passed turn. A number the count moved past, or of an older epoch, is
// refused with ErrLost. ctx ends a wait with its error.
func (o *Order) Await(ctx context.Context, at Place) (*Turn, error) {
	if at.epoch == 0 || at.seq == 0 {
		return nil, errors.New("browser: a tab action's epoch and number start at 1")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return PassedTurn(), nil
	}
	switch {
	case at.epoch < o.epoch:
		return nil, ErrLost
	case at.epoch > o.epoch:
		o.beginLocked(at.epoch)
	}
	seq := at.seq
	if seq < o.next {
		if _, skipped := o.outstanding[seq]; skipped {
			delete(o.outstanding, seq)
			return nil, ErrLost
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
		if o.ended {
			if err != nil {
				return nil, err
			}
			return PassedTurn(), nil
		}
		if o.epoch != at.epoch {
			// A newer epoch began, which dropped this one's waiters.
			if err != nil {
				return nil, err
			}
			return nil, ErrLost
		}
		if err != nil {
			delete(o.waiting, seq)
			return nil, err
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
	return &Turn{order: o, place: at}, nil
}

// beginLocked starts epoch, newer than the current one: the count begins
// again, the older epoch's waiters are refused and its turns pass.
func (o *Order) beginLocked(epoch Epoch) {
	o.epoch = epoch
	o.next, o.taken = 1, false
	o.waiting = make(map[Seq]struct{})
	o.outstanding = make(map[Seq]struct{})
	o.lostThrough = 0
	o.wakeLocked()
}

// End releases the waiters as Passed and makes every admitted turn commit
// Passed: the mount detached. The outstanding record goes with it.
func (o *Order) End() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.ended {
		o.ended = true
		o.waiting = nil
		o.outstanding = nil
		o.wakeLocked()
	}
}

// Stats is what an Order holds, for tests and diagnostics.
type Stats struct {
	Waiting, Outstanding int
	Taken                bool
}

// Stats answers what o holds now.
func (o *Order) Stats() Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return Stats{Waiting: len(o.waiting), Outstanding: len(o.outstanding), Taken: o.taken}
}

// Commit ends t and answers how it stands: Passed when its mount ended or
// a newer epoch began after admitting it, or when t already ended, and the
// caller then acts on nothing.
func (t *Turn) Commit() Standing {
	if t.order == nil {
		if t.done {
			return Passed
		}
		t.done = true
		return t.unorderedStanding()
	}
	o := t.order
	o.mu.Lock()
	defer o.mu.Unlock()
	if t.done {
		return Passed
	}
	o.endLocked(t)
	return o.standingLocked(t)
}

// Standing answers how t would stand if it committed now, without ending it.
func (t *Turn) Standing() Standing {
	if t.order == nil {
		if t.done {
			return Passed
		}
		return t.unorderedStanding()
	}
	t.order.mu.Lock()
	defer t.order.mu.Unlock()
	if t.done {
		return Passed
	}
	return t.order.standingLocked(t)
}

func (t *Turn) unorderedStanding() Standing {
	if t.unordered {
		return InOrder
	}
	return Passed
}

// standingLocked is how t, admitted and not ended, stands now.
func (o *Order) standingLocked(t *Turn) Standing {
	if o.ended || t.place.epoch != o.epoch {
		return Passed
	}
	return InOrder
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
	if o.taken && o.epoch == t.place.epoch && o.next == t.place.seq {
		o.taken = false
		o.next = t.place.seq + 1
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

// skipToLocked moves the count to low, recording the numbers it passes as
// outstanding. Past MaxSkipped the lowest are dropped into lostThrough.
func (o *Order) skipToLocked(low Seq) {
	from := o.next
	if low-from > MaxSkipped {
		from = low - MaxSkipped
		o.lostThrough = max(o.lostThrough, from-1)
	}
	for seq := from; seq < low; seq++ {
		if len(o.outstanding) >= MaxSkipped {
			var lowest Seq
			for s := range o.outstanding {
				if lowest == 0 || s < lowest {
					lowest = s
				}
			}
			delete(o.outstanding, lowest)
			o.lostThrough = max(o.lostThrough, lowest)
		}
		o.outstanding[seq] = struct{}{}
	}
	o.next = low
	o.wakeLocked()
}
