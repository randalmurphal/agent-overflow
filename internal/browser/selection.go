package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/browser/turnorder"
)

// The order of the user's tab commands. A pane's selecting actions (a tab
// chosen, a step, and the new, reopen, duplicate and download-open that
// select the tab they open) and the keyboard's commands on the active tab
// (CloseActiveCompanionPage, ReloadActiveCompanionPage) carry the pane's
// mount and a number the client gave when the user acted (Numbered). Each
// mount admits its numbers through its own turnorder.Order, whose package
// comment gives the rules. Here:
//
//   - A selection's or an opening's turn ends in the m.mu hold that records
//     the active tab or adds the tab (Turn.Commit there), so page work, a
//     restore or a navigation, never delays the next turn.
//   - A keyboard command on the active tab requires a number, resolves the
//     active tab in the m.mu hold that ends its turn, and refuses with
//     ActiveOutOfOrder, acting on nothing, when its turn stands OutOfOrder.
//     A selection names its tab or steps from what has arrived and acts
//     whatever its standing.
//   - A number from a mount the Manager does not have (detached, never
//     attached, or another thread's) is Passed: a quiet success that
//     selects, opens and restores nothing. A detach ends the mount's Order.
//
// A selection with no number, an agent's browser_select_page, the chat
// header's show, the pane's restore of the tab it shows
// (RestoreActiveCompanionPage), which selects nothing, and closing a tab by
// its id, which reselects only when the active tab is gone
// (repairActivePage), keep arrival order and never wait for a numbered one.

// MountID names one mounted browser pane (AttachPane).
type MountID string

// Numbered is a command's place in its pane mount's order. Only NewNumbered
// makes one, so every Numbered has a mount and a number from 1.
type Numbered struct {
	mount MountID
	seq   turnorder.Seq
}

// NewNumbered answers the place of the seq-th command made in mount.
func NewNumbered(mount MountID, seq uint64) (Numbered, error) {
	if strings.TrimSpace(string(mount)) == "" || seq == 0 {
		return Numbered{}, fmt.Errorf("browser: a numbered tab action needs its pane mount and a number from 1")
	}
	return Numbered{mount: mount, seq: turnorder.Seq(seq)}, nil
}

// Mount is the pane mount n was made in.
func (n Numbered) Mount() MountID { return n.mount }

type selectionKey struct{}

// WithSelection returns ctx carrying the user's numbered selection, which
// every Manager call that selects a tab for the user reads
// (ActivateCompanionPage, StepCompanionPage and the user's tab openings).
// Without it a selection keeps arrival order.
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

// awaitTurn admits sel in its mount's order for threadID. A mount the
// Manager does not have, or another thread's, answers a Passed turn. The
// caller commits the turn in the m.mu hold that acts and releases it when
// done.
func (m *Manager) awaitTurn(ctx context.Context, threadID string, sel Numbered) (*turnorder.Turn, error) {
	m.mu.Lock()
	mount, ok := m.panes[string(sel.mount)]
	if !ok || mount.threadID != threadID {
		m.mu.Unlock()
		return turnorder.PassedTurn(), nil
	}
	if mount.order == nil {
		mount.order = turnorder.New(m.selectionGapTimer)
		m.panes[string(sel.mount)] = mount
	}
	order := mount.order
	m.mu.Unlock()
	turn, err := order.Await(ctx, sel.seq)
	if err == nil && m.selectionAdmitted != nil && turn.Standing() != turnorder.Passed {
		m.selectionAdmitted(sel)
	}
	return turn, err
}

// selectionTurn is the turn of ctx's numbered selection, or an unordered
// turn for none.
func (m *Manager) selectionTurn(ctx context.Context, threadID string) (*turnorder.Turn, error) {
	if sel, ok := selectionOf(ctx); ok {
		return m.awaitTurn(ctx, threadID, sel)
	}
	return turnorder.Unordered(), nil
}

// wrapTab is index i of a strip of n tabs, wrapping both ways.
func wrapTab(i, n int) int {
	return (i%n + n) % n
}
