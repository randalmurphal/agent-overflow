package browser

import (
	"context"
	"fmt"
	"time"
)

// The order of the user's tab selections. Each companion action runs on its
// own goroutine, so two selections made in a row can reach the Manager in
// either order. A pane's selecting actions (a tab chosen, a step, and the
// new, reopen, duplicate and download-open that select the tab they open)
// carry the pane's mount and a number the client gave when the user acted,
// consecutive from 1 in each mount (Selection). The Manager admits them in
// that order, so each does what it says and nothing is replayed or skipped
// by rule:
//
//   - The next number takes its turn at once; its selection part (the
//     m.mu hold that picks and records the active tab, and for an opening
//     the hold that adds the tab) runs, and the count advances. Page work,
//     a restore or a navigation, runs after the turn, outside it.
//   - A higher number waits for its predecessors' selection parts, never for
//     their page work. A predecessor that never arrives (its request failed
//     in transit) is given up on once no turn has been taken for
//     selectionGapWait: the count moves to the lowest number waiting, and
//     the numbers skipped are passed.
//   - A number already passed, a repeat of one admitted or waiting, and any
//     number from a mount the Manager does not have (detached, never
//     attached, or another thread's) is a quiet success that selects,
//     opens and restores nothing: that pane is gone or the action already
//     happened. A detach releases the mount's waiters the same way.
//   - Past maxSelectionWaiters waiting in one mount a selection is refused
//     with an error and selects nothing; its number then becomes a gap.
//
// A selection with no number (an older client) or no mount, an agent's
// browser_select_page, the chat header's show, the pane's restore of the
// tab it shows (RestoreActiveCompanionPage), which selects nothing, and
// closing the active tab, which reselects only when the active tab is gone
// (repairActivePage), keep arrival order and never wait for a numbered one.

// Selection is a user's tab selection, numbered in the pane mount it was
// made in. A zero Seq is unnumbered.
type Selection struct {
	Mount string
	Seq   uint64
}

type selectionKey struct{}

// WithSelection returns ctx carrying the user's numbered selection, which
// every Manager call that selects a tab for the user reads
// (ActivateCompanionPage, StepCompanionPage, and the user's tab openings).
func WithSelection(ctx context.Context, sel Selection) context.Context {
	return context.WithValue(ctx, selectionKey{}, sel)
}

func selectionOf(ctx context.Context) Selection {
	sel, _ := ctx.Value(selectionKey{}).(Selection)
	return sel
}

const (
	// selectionGapWait is how long waiting selections wait, with no turn
	// taken, for a predecessor before giving it up.
	selectionGapWait = time.Second
	// maxSelectionWaiters bounds the selections waiting in one mount.
	maxSelectionWaiters = 256
)

// mountSelection is one mount's admission state. Guarded by Manager.mu.
type mountSelection struct {
	// next is the number admitted next.
	next uint64
	// taken is set while next holds its turn: admitted, its selection part
	// not yet done.
	taken bool
	// waiting holds the numbers above next waiting for their turn.
	waiting map[uint64]struct{}
	// gone is set when the mount detached.
	gone bool
	// changed is closed, and replaced, on every change a waiter waits for.
	changed chan struct{}
}

func (o *mountSelection) wakeLocked() {
	close(o.changed)
	o.changed = make(chan struct{})
}

// lowestWaitingLocked is the lowest number waiting, 0 for none.
func (o *mountSelection) lowestWaitingLocked() uint64 {
	var low uint64
	for seq := range o.waiting {
		if low == 0 || seq < low {
			low = seq
		}
	}
	return low
}

// selectionTurn is an admitted selection's turn. The selecting m.mu hold
// ends it (landedLocked); the caller ends it anyway once it is done
// (finishSelectionTurn), so a selection that failed before selecting still
// lets the next one in. A turn with no order is an unordered selection.
type selectionTurn struct {
	order  *mountSelection
	seq    uint64
	passed bool
	done   bool
}

// landedLocked ends the turn: the next number may take its own. The caller
// holds m.mu. Repeated calls do nothing.
func (t *selectionTurn) landedLocked() {
	if t == nil || t.order == nil || t.done {
		return
	}
	t.done = true
	if o := t.order; o.taken && o.next == t.seq {
		o.taken = false
		o.next = t.seq + 1
		o.wakeLocked()
	}
}

func (m *Manager) finishSelectionTurn(t *selectionTurn) {
	if t == nil || t.order == nil {
		return
	}
	m.mu.Lock()
	t.landedLocked()
	m.mu.Unlock()
}

// awaitSelectionTurn admits sel, a selection for threadID: it answers once
// sel's turn has come, or at once with a passed turn, which must select
// nothing, or for an unordered selection. The caller selects in an m.mu
// hold that calls the turn's landedLocked, and calls finishSelectionTurn
// when done. ctx ends a wait with its error.
func (m *Manager) awaitSelectionTurn(ctx context.Context, threadID string, sel Selection) (*selectionTurn, error) {
	if sel.Seq == 0 || sel.Mount == "" {
		return &selectionTurn{}, nil
	}
	passed := &selectionTurn{passed: true}
	m.mu.Lock()
	mount, ok := m.panes[sel.Mount]
	if !ok || mount.threadID != threadID {
		m.mu.Unlock()
		return passed, nil
	}
	order := mount.selection
	if order == nil {
		order = &mountSelection{next: 1, waiting: make(map[uint64]struct{}), changed: make(chan struct{})}
		mount.selection = order
		m.panes[sel.Mount] = mount
	}
	_, repeat := order.waiting[sel.Seq]
	if sel.Seq < order.next || (sel.Seq == order.next && order.taken) || repeat {
		m.mu.Unlock()
		return passed, nil
	}
	if sel.Seq > order.next {
		if len(order.waiting) >= maxSelectionWaiters {
			m.mu.Unlock()
			return nil, fmt.Errorf("browser: too many tab selections waiting in this pane")
		}
		order.waiting[sel.Seq] = struct{}{}
	}
	for sel.Seq != order.next || order.taken {
		changed := order.changed
		m.mu.Unlock()
		expired, stop := m.selectionGapTimer()
		var err error
		select {
		case <-changed:
		case <-expired:
		case <-ctx.Done():
			err = ctx.Err()
		}
		stop()
		m.mu.Lock()
		if err != nil || order.gone {
			delete(order.waiting, sel.Seq)
			m.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return passed, nil
		}
		select {
		case <-changed:
		default:
			// No turn was taken for the whole wait: the predecessors
			// missing below the lowest number waiting are given up.
			if low := order.lowestWaitingLocked(); !order.taken && low > order.next {
				order.next = low
				order.wakeLocked()
			}
		}
	}
	delete(order.waiting, sel.Seq)
	order.taken = true
	m.mu.Unlock()
	return &selectionTurn{order: order, seq: sel.Seq}, nil
}

// endSelectionLocked releases a detaching mount's waiters as passed. The
// caller holds m.mu.
func (mount paneMount) endSelectionLocked() {
	if o := mount.selection; o != nil && !o.gone {
		o.gone = true
		o.wakeLocked()
	}
}

// newSelectionGapTimer is the default Manager.selectionGapTimer.
func newSelectionGapTimer() (<-chan time.Time, func() bool) {
	timer := time.NewTimer(selectionGapWait)
	return timer.C, timer.Stop
}

// wrapTab is index i of a strip of n tabs, wrapping both ways.
func wrapTab(i, n int) int {
	return (i%n + n) % n
}
