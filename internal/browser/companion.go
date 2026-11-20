package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/browser/turnorder"
)

// MoveCompanionPage places one of the thread's tabs, in any state, at
// index in tab order (clamped). Moving a tab is not a touch. A suspended
// page's new position is saved with it. From a pane it carries its numbered
// place in the pane's order (ctx, WithSelection) and moves the tab in the
// m.mu hold that ends its turn, so a close that reads the strip's order
// (close to the right) acts on the order the user saw when they asked for
// it; a passed turn moves nothing.
func (m *Manager) MoveCompanionPage(ctx context.Context, access Access, pageID PageID, index int) error {
	if err := m.bound(access); err != nil {
		return err
	}
	pageID = pageID.trim()
	turn, err := m.selectionTurn(ctx, access)
	if err != nil {
		return err
	}
	defer turn.Release()
	if err := m.lockStructure(ctx, access.tb); err != nil {
		return err
	}
	if turn.Commit() == turnorder.Passed {
		m.mu.Unlock()
		return nil
	}
	if err := access.admitLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	savedMoved, found := m.placeTabLocked(access.tb, pageID, index)
	m.mu.Unlock()
	if !found {
		return errPageNotFound
	}
	m.emitThreadState(access.ThreadID())
	if savedMoved {
		return m.persistThreads([]ThreadID{access.ThreadID()})
	}
	return nil
}

// placeTabLocked places tb's tab pageID at index in tab order (clamped),
// and reports whether a suspended tab's saved position changed and whether
// the tab exists. The moved prefix is renumbered 1..n, and a page opened
// later keeps appending at the end because its creation-time key is always
// larger. The caller holds m.mu.
func (m *Manager) placeTabLocked(tb *threadBrowser, pageID PageID, index int) (savedMoved, found bool) {
	tabs := m.companionTabsLocked(tb)
	ordered := make([]threadTab, 0, len(tabs))
	var moved *threadTab
	for i := range tabs {
		if tabs[i].id == pageID {
			moved = &tabs[i]
		} else {
			ordered = append(ordered, tabs[i])
		}
	}
	if moved == nil {
		return false, false
	}
	index = max(0, min(index, len(ordered)))
	ordered = append(ordered[:index], append([]threadTab{*moved}, ordered[index:]...)...)
	for i, tab := range ordered {
		if tab.live != nil {
			tab.live.tabOrder.Store(int64(i + 1))
		} else if tab.pending != nil {
			tab.pending.p.tabOrder.Store(int64(i + 1))
		} else if tab.rec.tabOrder != int64(i+1) {
			tab.rec.tabOrder = int64(i + 1)
			savedMoved = true
		}
	}
	return savedMoved, true
}

// NewCompanionPage is the user opening a tab: it is selected and the
// companion shown while the engine page is created, and a failed creation
// stays as the tab's error (createPage). A non-empty address is then
// navigated as the address bar would. It answers the new page, even when its
// navigation failed; an empty ID with no error when the tab was closed while
// it was created or shows why it could not be, or when ctx's numbered
// selection was passed (selection.go) and nothing opened.
func (m *Manager) NewCompanionPage(ctx context.Context, access Access, address string) (PageInfo, error) {
	if err := m.bound(access); err != nil {
		return PageInfo{}, err
	}
	turn, err := m.selectionTurn(ctx, access)
	if err != nil || turn.Standing() == turnorder.Passed {
		return PageInfo{}, err
	}
	defer turn.Release()
	info, err := m.openUserTab(ctx, access, turn, address, "", -1, nil)
	if errors.Is(err, errSelectionPassed) || errors.Is(err, errTabReplaced) {
		// Nothing to report: the pane detached, or the new tab was closed or
		// replaced before its address loaded, which then loads nowhere.
		return PageInfo{}, nil
	}
	return info, err
}

// openUserTab is NewCompanionPage, in turn (selectionTurn), with the tab
// placed at index in tab order (negative appends it), given label and
// taking claim (createPageAt). It answers errSelectionPassed, opening
// nothing, when the turn's pane detached before the tab was added. The
// label and the address go to the page the creation made, never to the
// tab's id (tabHold): when a jar switch, a close or a forget of the
// claim's browsing came first, they go nowhere, and it answers
// errTabReplaced or errClaimVoid.
func (m *Manager) openUserTab(ctx context.Context, access Access, turn *turnorder.Turn, address, label string, index int, claim *tabClaim) (PageInfo, error) {
	if strings.TrimSpace(access.Workspace()) == "" {
		return PageInfo{}, fmt.Errorf("browser: invalid caller scope")
	}
	p, err := m.createPageAt(ctx, access, turn, index, claim)
	var onTab *shownOnTab
	if errors.Is(err, errClosedWhileCreating) || errors.As(err, &onTab) {
		return PageInfo{}, nil
	}
	if err != nil {
		return PageInfo{}, err
	}
	hold := tabHold{p: p, claim: claim}
	if label != "" {
		// A closed tab's label, unless another tab took it meanwhile, which
		// keeps it (labelHolderLocked). The tab was pushed without it, and
		// a navigation that fails before its operation would not push it.
		// A relabel waits out a jar switch (lockHeld); no cancel, a switch
		// ends with its one write.
		if err := m.lockHeld(context.Background(), access, hold); err != nil {
			return p.cachedInfo(), err
		}
		if m.labelHolderLocked(p.owner, p.id, label) == "" {
			p.commitInfo(pageUpdate{Label: &label})
		}
		m.mu.Unlock()
		m.publishPages(p.owner.id)
	}
	if strings.TrimSpace(address) != "" {
		if _, err := m.navigateUserAddress(ctx, access, OpenOptions{tab: &hold}, address); err != nil {
			return p.cachedInfo(), err
		}
	}
	return p.cachedInfo(), nil
}

// ActivateCompanionPage is the user selecting a tab in the pane. It is
// presented first (selectTab); a suspended page is then restored, and the
// call answers when it is. A tab closed while it restored is no error.
// ctx's numbered selection takes its turn in the user's order first
// (selection.go); a passed one changes and restores nothing.
func (m *Manager) ActivateCompanionPage(ctx context.Context, access Access, pageID PageID) error {
	if err := m.bound(access); err != nil {
		return err
	}
	turn, err := m.selectionTurn(ctx, access)
	if err != nil {
		return err
	}
	selected, err := m.selectTab(access, turn, pageID.trim())
	if err != nil || selected == "" {
		return err
	}
	return m.restoreSelectedTab(ctx, access, selected)
}

// RestoreActiveCompanionPage is the pane asking for the suspended tab it
// shows, the thread's active one, to be restored. It selects nothing, so it
// cannot undo a selection the user made since the pane saw that tab; a tab
// that is no longer active is left as it is.
func (m *Manager) RestoreActiveCompanionPage(ctx context.Context, access Access, pageID PageID) error {
	if err := m.bound(access); err != nil {
		return err
	}
	pageID = pageID.trim()
	if err := access.End(); err != nil {
		return err
	}
	m.mu.Lock()
	active := access.tb.session.activePage()
	m.mu.Unlock()
	if pageID == "" || pageID != active {
		return nil
	}
	return m.restoreSelectedTab(ctx, access, pageID)
}

// StepCompanionPage is the user's next/previous tab: it selects the tab step
// places from the active one in strip order, wrapping, as one change to the
// Manager's own order and selection, so presses in a row each move one tab
// whatever the client last saw. With no active tab, a forward step selects
// the first tab and a backward one the last. Then it is ActivateCompanionPage,
// in the user's order with the mount's other selections (selection.go).
func (m *Manager) StepCompanionPage(ctx context.Context, access Access, step int) error {
	if err := m.bound(access); err != nil {
		return err
	}
	turn, err := m.selectionTurn(ctx, access)
	if err != nil {
		return err
	}
	selected, err := m.selectTabWhere(access, turn, func(tabs []threadTab, active PageID) int {
		n := len(tabs)
		if n == 0 || step == 0 {
			return -1
		}
		at := -1
		for i := range tabs {
			if tabs[i].id == active {
				at = i
			}
		}
		if at < 0 && step < 0 {
			at = 0
		}
		return wrapTab(at+step, n)
	})
	if step == 0 {
		// Refused inside the turn, so the numbers after it do not wait.
		return fmt.Errorf("browser: tab step must not be 0")
	}
	if err != nil || selected == "" {
		return err
	}
	return m.restoreSelectedTab(ctx, access, selected)
}

// restoreSelectedTab restores the selected tab when it is suspended and
// answers once it is; a tab closed meanwhile is no error. A tab whose page
// crashed comes back by its Reload only (ReloadCompanionPage), so selecting
// it does not load a page that may crash again.
func (m *Manager) restoreSelectedTab(ctx context.Context, access Access, pageID PageID) error {
	m.mu.Lock()
	rec := m.suspended[pageID]
	crashed := rec != nil && rec.failure != ""
	m.mu.Unlock()
	if rec == nil || crashed {
		return nil
	}
	_, _, err := m.resolvePage(ctx, access, pageID, false)
	if err != nil && errors.Is(err, errPageNotFound) && !m.hasTab(access.tb, pageID) {
		return nil
	}
	return err
}
