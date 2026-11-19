package browser

import (
	"context"

	"agent-overflow/internal/browser/turnorder"
)

// The keyboard's commands on the active tab: close (Mod+W) and reload
// (Mod+R). Each names no tab. It takes its numbered place in its pane
// mount's order (selection.go), and the m.mu hold that commits its turn
// reads the tab active then, after the user's earlier selections and
// commands, never after their page work. A close takes that tab out of the
// tab set and picks the next active one in the same hold, so the next turn
// sees both; a reload binds that tab and reloads it after the turn, and
// never another one. The tab menu's and the address bar's commands name
// their tab and keep arrival order.

// ActiveOutcome is what a keyboard command on the active tab did.
type ActiveOutcome int

const (
	// ActivePassed acted on nothing: a repeat of its number, or its pane
	// is gone.
	ActivePassed ActiveOutcome = iota
	// ActiveApplied acted on the tab active at its turn.
	ActiveApplied
	// ActiveNoTab acted on nothing: the thread had no tab, or a reload's
	// tab closed before the reload ran.
	ActiveNoTab
	// ActiveOutOfOrder acted on nothing: an earlier command of its pane had
	// not arrived when its turn came, so the active tab was not known
	// (turnorder.OutOfOrder).
	ActiveOutOfOrder
)

// CloseActiveCompanionPage closes the tab active at at's turn. A tab still
// opening or restoring closes at once; its page work and the teardown never
// delay the next turn.
func (m *Manager) CloseActiveCompanionPage(ctx context.Context, access Access, at Numbered) (ActiveOutcome, error) {
	turn, err := m.awaitTurn(ctx, access, at)
	if err != nil {
		return ActivePassed, err
	}
	defer turn.Release()
	if err := m.lockStructure(ctx, access.tb); err != nil {
		return ActivePassed, err
	}
	outcome, finish := m.closeActiveLocked(access, turn)
	m.mu.Unlock()
	if outcome != ActiveApplied {
		return outcome, nil
	}
	return outcome, finish(ctx, false)
}

// closeActiveLocked is CloseActiveCompanionPage's commit: it ends turn and,
// when the turn acts, takes the active tab out of the tab set with the next
// active tab picked, and answers the rest of the close (closeTabLocked),
// which the caller runs after releasing m.mu. The caller holds m.mu, taken
// by lockStructure for access's lifetime.
func (m *Manager) closeActiveLocked(access Access, turn *turnorder.Turn) (ActiveOutcome, func(ctx context.Context, wait bool) error) {
	outcome, pageID := m.commitActiveLocked(access, turn)
	if outcome != ActiveApplied {
		return outcome, nil
	}
	finish := m.closeTabLocked(access.tb, pageID, true)
	m.repairActivePageLocked(access.tb)
	return outcome, finish
}

// ReloadActiveCompanionPage reloads the tab active at at's turn, as
// ReloadCompanionPage reloads it, after the turn.
func (m *Manager) ReloadActiveCompanionPage(ctx context.Context, access Access, at Numbered) (ActiveOutcome, error) {
	turn, err := m.awaitTurn(ctx, access, at)
	if err != nil {
		return ActivePassed, err
	}
	defer turn.Release()
	m.mu.Lock()
	outcome, pageID := m.commitActiveLocked(access, turn)
	m.mu.Unlock()
	if outcome != ActiveApplied {
		return outcome, nil
	}
	if err := m.ReloadCompanionPage(ctx, access, pageID); err != nil {
		if !m.hasTab(access.tb, pageID) {
			// The tab closed after the turn (a Mod+W pressed next).
			return ActiveNoTab, nil
		}
		return outcome, err
	}
	return outcome, nil
}

// commitActiveLocked commits turn and answers what its command acts on: the
// tab access's lifetime shows (shownTabLocked) for a turn InOrder, nothing
// otherwise. A lifetime the thread's close detached acts on nothing, as a
// Passed turn does. The caller holds m.mu.
func (m *Manager) commitActiveLocked(access Access, turn *turnorder.Turn) (ActiveOutcome, string) {
	standing := turn.Commit()
	if access.admitLocked() != nil {
		return ActivePassed, ""
	}
	switch standing {
	case turnorder.Passed:
		return ActivePassed, ""
	case turnorder.OutOfOrder:
		return ActiveOutOfOrder, ""
	case turnorder.InOrder:
		if pageID := m.shownTabLocked(access.tb); pageID != "" {
			return ActiveApplied, pageID
		}
		return ActiveNoTab, ""
	default:
		// A standing this switch does not know acts on nothing.
		return ActiveOutOfOrder, ""
	}
}

// shownTabLocked is the tab the pane shows of tb: the active tab, or with
// none the first in strip order; "" when tb has no tab. The caller holds
// m.mu.
func (m *Manager) shownTabLocked(tb *threadBrowser) string {
	tabs := m.companionTabsLocked(tb)
	active := tb.session.ActivePageID
	for _, tab := range tabs {
		if tab.id == active {
			return active
		}
	}
	if len(tabs) > 0 {
		return tabs[0].id
	}
	return ""
}
