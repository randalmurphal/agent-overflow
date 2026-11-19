package browser

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The order of the user's tab selections. Each companion action runs on its
// own goroutine, so two selections made in a row can reach the Manager in
// either order. A pane's selecting actions (a tab chosen, a step, and the
// new, reopen, duplicate and download-open that select the tab they open)
// and the keyboard's commands on the active tab, which name no tab and act
// on the one active at their commit (CloseActiveCompanionPage), carry the
// pane's mount and a number the client gave when the user acted,
// consecutive from 1 in each mount (Selection). The Manager admits them in
// that order, so each does what it says and nothing is replayed or skipped
// by rule:
//
//   - The next number takes its turn at once. Its commit (the m.mu hold
//     that picks and records the active tab, or for an opening the hold
//     that adds the tab) ends the turn through commitSelectionLocked, and
//     the count advances. Page work, a restore or a navigation, runs after
//     the commit, outside the turn.
//   - A higher number waits for its predecessors' commits, never for their
//     page work. When no turn has been taken for selectionGapWait the count
//     moves to the lowest number waiting. The numbers it moved past lose
//     their place, not their effect: they are recorded (maxSkippedSelections
//     per mount), and one that arrives later is admitted at once, in
//     arrival order, as an unnumbered selection would be. A skipped number
//     the record no longer holds is refused with an error when it arrives;
//     so is a repeat of an admitted number at or below the highest one
//     dropped, which the record cannot tell from it.
//   - A repeat of a number admitted or waiting, and any number from a mount
//     the Manager does not have (detached, never attached, or another
//     thread's), is a quiet success that selects, opens and restores
//     nothing: the action already happened, or its pane is gone. A detach
//     releases the mount's waiters the same way, and a turn admitted before
//     its mount detached commits nothing (commitSelectionLocked).
//   - Past maxSelectionWaiters waiting in one mount a selection is refused
//     with an error and selects nothing. Its number becomes a gap: sent
//     again before the count passes it, it waits its turn; after, it is a
//     skipped number.
//
// A selection with no number (an older client) or no mount, an agent's
// browser_select_page, the chat header's show, the pane's restore of the
// tab it shows (RestoreActiveCompanionPage), which selects nothing, and
// closing a tab by its id, which reselects only when the active tab is gone
// (repairActivePage), keep arrival order and never wait for a numbered one.

// Selection is a user's tab selection, numbered in the pane mount it was
// made in. A zero Seq is unnumbered.
type Selection struct {
	Mount string
	Seq   uint64
}

type selectionKey struct{}

// WithSelection returns ctx carrying the user's numbered selection, which
// every Manager call that selects a tab for the user or acts on the active
// one for the keyboard reads (ActivateCompanionPage, StepCompanionPage, the
// user's tab openings and CloseActiveCompanionPage).
func WithSelection(ctx context.Context, sel Selection) context.Context {
	return context.WithValue(ctx, selectionKey{}, sel)
}

func selectionOf(ctx context.Context) Selection {
	sel, _ := ctx.Value(selectionKey{}).(Selection)
	return sel
}

const (
	// selectionGapWait is how long waiting selections wait, with no turn
	// taken, for a predecessor before it loses its place.
	selectionGapWait = time.Second
	// maxSelectionWaiters bounds the selections waiting in one mount.
	maxSelectionWaiters = 256
	// maxSkippedSelections bounds the numbers a mount remembers as skipped.
	maxSkippedSelections = 256
)

// errSelectionPassed is a selection's commit finding its turn's mount gone:
// it acts on nothing, and the caller answers success.
var errSelectionPassed = errors.New("browser: the pane this tab action came from is gone")

// errSelectionLost answers a skipped number the mount no longer remembers.
var errSelectionLost = errors.New("browser: this tab action arrived too late to apply; try it again")

// mountSelection is one mount's admission state. Guarded by Manager.mu.
type mountSelection struct {
	// next is the number admitted next.
	next uint64
	// taken is set while next holds its turn: admitted, not yet committed.
	taken bool
	// waiting holds the numbers above next waiting for their turn.
	waiting map[uint64]struct{}
	// skipped holds numbers below next the count moved past before they
	// arrived. lostThrough is the highest one dropped from it at the bound.
	skipped     map[uint64]struct{}
	lostThrough uint64
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

// skipToLocked moves the count to low, recording the numbers it passes.
// Past maxSkippedSelections the lowest are dropped into lostThrough.
func (o *mountSelection) skipToLocked(low uint64) {
	from := o.next
	if low-from > maxSkippedSelections {
		from = low - maxSkippedSelections
		o.lostThrough = max(o.lostThrough, from-1)
	}
	for seq := from; seq < low; seq++ {
		if len(o.skipped) >= maxSkippedSelections {
			var lowest uint64
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
	o.wakeLocked()
}

// selectionTurn is an admitted selection's turn. The m.mu hold that commits
// the selection ends it through commitSelectionLocked; the caller releases
// it anyway once done (releaseSelectionTurn), so a selection that failed
// before committing still lets the next one in. A turn with no order is an
// unordered selection. A skipped number admitted on arrival is below next,
// so ending its turn moves nothing.
type selectionTurn struct {
	order  *mountSelection
	seq    uint64
	passed bool
	done   bool
}

// commitSelectionLocked ends t and answers whether its commit may act: false
// when t's mount detached after admitting it, and the commit then selects,
// opens and consumes nothing. Every commit of a user's selection calls it in
// the m.mu hold that commits. A turn already ended answers false.
func (m *Manager) commitSelectionLocked(t *selectionTurn) bool {
	if t.done {
		return false
	}
	if t.order == nil {
		t.done = true
		return true
	}
	m.endTurnLocked(t)
	return !t.order.gone
}

// liveSelectionLocked is whether t may still commit, without ending it.
func liveSelectionLocked(t *selectionTurn) bool {
	return !t.done && (t.order == nil || !t.order.gone)
}

func (m *Manager) endTurnLocked(t *selectionTurn) {
	t.done = true
	if o := t.order; o.taken && o.next == t.seq {
		o.taken = false
		o.next = t.seq + 1
		o.wakeLocked()
	}
}

// releaseSelectionTurn ends t if no commit did.
func (m *Manager) releaseSelectionTurn(t *selectionTurn) {
	if t == nil || t.order == nil {
		return
	}
	m.mu.Lock()
	if !t.done {
		m.endTurnLocked(t)
	}
	m.mu.Unlock()
}

// awaitSelectionTurn admits sel, a selection for threadID: it answers once
// sel's turn has come, at once for an unordered selection or a skipped
// number arriving late, or at once with a passed turn, which must act on
// nothing. The caller commits in an m.mu hold that calls
// commitSelectionLocked, and calls releaseSelectionTurn when done. ctx ends
// a wait with its error.
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
		order = &mountSelection{
			next:    1,
			waiting: make(map[uint64]struct{}),
			skipped: make(map[uint64]struct{}),
			changed: make(chan struct{}),
		}
		mount.selection = order
		m.panes[sel.Mount] = mount
	}
	if sel.Seq < order.next {
		if _, skipped := order.skipped[sel.Seq]; skipped {
			delete(order.skipped, sel.Seq)
			m.mu.Unlock()
			return m.admitted(sel, &selectionTurn{order: order, seq: sel.Seq}), nil
		}
		lost := sel.Seq <= order.lostThrough
		m.mu.Unlock()
		if lost {
			return nil, errSelectionLost
		}
		return passed, nil
	}
	if _, repeat := order.waiting[sel.Seq]; repeat || (sel.Seq == order.next && order.taken) {
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
			// No turn was taken for the whole wait: the numbers missing
			// below the lowest one waiting lose their place.
			if low := order.lowestWaitingLocked(); !order.taken && low > order.next {
				order.skipToLocked(low)
			}
		}
	}
	delete(order.waiting, sel.Seq)
	order.taken = true
	m.mu.Unlock()
	return m.admitted(sel, &selectionTurn{order: order, seq: sel.Seq}), nil
}

// admitted answers turn, after the test seam that runs between a turn's
// admission and its commit.
func (m *Manager) admitted(sel Selection, turn *selectionTurn) *selectionTurn {
	if m.selectionAdmitted != nil {
		m.selectionAdmitted(sel)
	}
	return turn
}

// endSelectionLocked releases a detaching mount's waiters as passed; its
// admitted turns then commit nothing. The caller holds m.mu.
func (mount paneMount) endSelectionLocked() {
	if o := mount.selection; o != nil && !o.gone {
		o.gone = true
		o.skipped = nil
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
