package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/browser/turnorder"
)

// The order of the user's tab commands. Every command of a pane that reads
// or changes the tab strip's selection, order or membership carries the
// pane's mount and a number the client gave when the user acted (Numbered):
// its selecting actions (a tab chosen, a step, and the new, reopen,
// duplicate, download-open and show that select the tab they open), its
// closes (CloseCompanionPage, CloseCompanionTabs) and its moves
// (MoveCompanionPage). Each mount admits its numbers through its own
// turnorder.Order, whose package comment gives the rules. Here:
//
//   - A selection's, an opening's, a close's or a move's turn ends in the
//     m.mu hold that records the active tab, adds the tab, takes the closed
//     tabs out of the tab set or reorders it (Turn.Commit there), so page
//     work, a restore or a navigation, never delays the next turn.
//   - Every one names its tab, or steps from what has arrived, so none
//     depends on which tab was active when an earlier one was sent: a close
//     names the tab the user was looking at when they acted and closes it
//     if it is still there. A number that arrives after the count passed
//     it, or of an older epoch, is refused with turnorder.ErrLost.
//   - A number from a mount the Manager does not have (detached, never
//     attached, another thread's, or another lifetime's of the thread) is
//     Passed: a quiet success that selects, opens, closes, moves and
//     restores nothing. A detach ends the mount's Order, and so does the
//     close of the thread lifetime the mount attached to (CloseThread), so
//     an earlier lifetime's mount still registered commits nothing.
//
// A selection with no number, an agent's browser_select_page, the chat
// header's show, the pane's restore of the tab it shows
// (RestoreActiveCompanionPage), which selects nothing, and an agent's close
// keep arrival order and never wait for a numbered one.

// Numbered is a command's place in its pane mount's order. Only NewNumbered
// makes one, so every Numbered has a mount and an epoch and number from 1.
type Numbered struct {
	mount MountID
	place turnorder.Place
}

// NewNumbered answers the place of the seq-th command of epoch made in
// mount.
func NewNumbered(mount MountID, epoch, seq uint64) (Numbered, error) {
	place, err := turnorder.NewPlace(epoch, seq)
	if strings.TrimSpace(string(mount)) == "" || err != nil {
		return Numbered{}, fmt.Errorf("browser: a numbered tab action needs its pane mount and an epoch and number from 1")
	}
	return Numbered{mount: mount, place: place}, nil
}

// Mount is the pane mount n was made in.
func (n Numbered) Mount() MountID { return n.mount }

type selectionKey struct{}

// WithSelection returns ctx carrying the user's numbered selection, which
// every Manager call that selects, opens, closes or moves a tab for the
// user reads (ActivateCompanionPage, StepCompanionPage, the user's tab
// openings, closes and MoveCompanionPage). Without it a command keeps
// arrival order.
func WithSelection(ctx context.Context, sel Numbered) context.Context {
	return context.WithValue(ctx, selectionKey{}, sel)
}

func selectionOf(ctx context.Context) (Numbered, bool) {
	sel, ok := ctx.Value(selectionKey{}).(Numbered)
	return sel, ok
}

// errSelectionPassed is a selection's commit finding its turn Passed: it
// acts on nothing, and the caller answers success.
var errSelectionPassed = errors.New("browser: the pane this tab action came from is gone")

// ErrMountGone refuses an action bound to a pane mount (BindMount) the
// Manager does not have: the pane detached, or never was the thread's.
var ErrMountGone = errors.New("browser: the browser pane this was chosen in has closed")

// BindMount binds an action to the thread lifetime pane mount attached to,
// for an action the user gave consent to in that pane (a jar switch), and
// then reads, through lookup, the Caller it acts as (BindThread). It
// reaches that lifetime and no later one: once the thread's close detached
// it, a commit through it answers ErrThreadClosed in the hold that checks
// the rest of the action (Access.admitLocked). A mount the Manager does not
// have, or another thread's, is refused with ErrMountGone.
func (m *Manager) BindMount(threadID ThreadID, mount MountID, lookup func() (Caller, error)) (Access, error) {
	threadID = threadID.trim()
	m.mu.Lock()
	pane, ok := m.panes[mount.trim()]
	m.mu.Unlock()
	if !ok || pane.owner == nil || pane.owner.id != threadID {
		return Access{}, ErrMountGone
	}
	caller, err := callerOf(threadID, lookup)
	if err != nil {
		return Access{}, err
	}
	return Access{caller: caller, tb: pane.owner}, nil
}

// awaitTurn admits sel in its mount's order for access's lifetime. A mount
// the Manager does not have, or another lifetime's, answers a Passed turn,
// and so does one whose lifetime was closed. The caller commits the turn in
// the m.mu hold that acts and releases it when done.
func (m *Manager) awaitTurn(ctx context.Context, access Access, sel Numbered) (*turnorder.Turn, error) {
	m.mu.Lock()
	mount, ok := m.mountLocked(access.tb, sel.mount)
	if !ok {
		m.mu.Unlock()
		return turnorder.PassedTurn(), nil
	}
	if mount.order == nil {
		mount.order = turnorder.New(m.selectionGapTimer)
		m.panes[sel.mount] = mount
	}
	order := mount.order
	m.mu.Unlock()
	turn, err := order.Await(ctx, sel.place)
	if err == nil && m.selectionAdmitted != nil && turn.Standing() != turnorder.Passed {
		m.selectionAdmitted(sel)
	}
	return turn, err
}

// selectionTurn is the turn of ctx's numbered selection, or an unordered
// turn for none.
func (m *Manager) selectionTurn(ctx context.Context, access Access) (*turnorder.Turn, error) {
	if sel, ok := selectionOf(ctx); ok {
		return m.awaitTurn(ctx, access, sel)
	}
	return turnorder.Unordered(), nil
}

// wrapTab is index i of a strip of n tabs, wrapping both ways.
func wrapTab(i, n int) int {
	return (i%n + n) % n
}
