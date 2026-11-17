package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MoveCompanionPage places one of the thread's tabs, in any state, at
// index in tab order (clamped). Moving a tab is not a touch. A suspended
// page's new position is saved with it.
func (m *Manager) MoveCompanionPage(access Access, pageID string, index int) error {
	pageID = strings.TrimSpace(pageID)
	m.mu.Lock()
	savedMoved, found := m.placeTabLocked(access.ThreadID, pageID, index)
	m.mu.Unlock()
	if !found {
		return errPageNotFound
	}
	m.emitThreadState(access.ThreadID)
	if savedMoved {
		return m.persistThreads([]string{access.ThreadID})
	}
	return nil
}

// placeTabLocked places the thread's tab pageID at index in tab order
// (clamped), and reports whether a suspended tab's saved position changed
// and whether the tab exists. The moved prefix is renumbered 1..n, and a
// page opened later keeps appending at the end because its creation-time
// key is always larger. The caller holds m.mu.
func (m *Manager) placeTabLocked(threadID, pageID string, index int) (savedMoved, found bool) {
	tabs := m.companionTabsLocked(threadID)
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
// it was created or shows why it could not be.
func (m *Manager) NewCompanionPage(ctx context.Context, access Access, address string) (PageInfo, error) {
	return m.openUserTab(ctx, access, address, "", -1)
}

// openUserTab is NewCompanionPage with the tab placed at index in tab order
// (negative appends it) and given label.
func (m *Manager) openUserTab(ctx context.Context, access Access, address, label string, index int) (PageInfo, error) {
	if strings.TrimSpace(access.ThreadID) == "" || strings.TrimSpace(access.Workspace) == "" {
		return PageInfo{}, fmt.Errorf("browser: invalid caller scope")
	}
	p, err := m.createPageAt(ctx, access, true, index)
	var onTab *shownOnTab
	if errors.Is(err, errClosedWhileCreating) || errors.As(err, &onTab) {
		return PageInfo{}, nil
	}
	if err != nil {
		return PageInfo{}, err
	}
	if label != "" {
		// A closed tab's label, unless another tab took it meanwhile, which
		// keeps it (labelHolderLocked). The tab was pushed without it, and
		// a navigation that fails before its operation would not push it.
		m.mu.Lock()
		if m.labelHolderLocked(p.owner, p.id, label) == "" {
			p.commitInfo(pageUpdate{Label: &label})
		}
		m.mu.Unlock()
		m.publishPages(p.owner)
	}
	if strings.TrimSpace(address) != "" {
		if _, err := m.NavigateCompanion(ctx, access, p.id, address); err != nil {
			return p.cachedInfo(), err
		}
	}
	return p.cachedInfo(), nil
}

// ActivateCompanionPage is the user selecting a tab in the pane. It is
// presented first (selectTab); a suspended page is then restored, and the
// call answers when it is. A tab closed while it restored is no error. A
// selection a newer one from its mount overtook (ctx's Selection,
// selection.go) changes nothing and restores nothing.
func (m *Manager) ActivateCompanionPage(ctx context.Context, access Access, pageID string) error {
	pageID = strings.TrimSpace(pageID)
	applied, err := m.selectTab(access, selectionOf(ctx), pageID)
	if err != nil || !applied {
		return err
	}
	return m.restoreSelectedTab(ctx, access, pageID)
}

// RestoreActiveCompanionPage is the pane asking for the suspended tab it
// shows, the thread's active one, to be restored. It selects nothing, so it
// cannot undo a selection the user made since the pane saw that tab; a tab
// that is no longer active is left as it is.
func (m *Manager) RestoreActiveCompanionPage(ctx context.Context, access Access, pageID string) error {
	pageID = strings.TrimSpace(pageID)
	m.mu.Lock()
	active := m.sessionLocked(access.ThreadID).ActivePageID
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
// the first tab and a backward one the last. Then it is ActivateCompanionPage.
// Numbered steps compose by addition in any order (selection.go).
func (m *Manager) StepCompanionPage(ctx context.Context, access Access, step int) error {
	if step == 0 {
		return fmt.Errorf("browser: tab step must not be 0")
	}
	pageID, applied, err := m.selectTabWhere(access, selectionOf(ctx), step, func(tabs []threadTab, active string) int {
		n := len(tabs)
		if n == 0 {
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
	if err != nil || !applied {
		return err
	}
	return m.restoreSelectedTab(ctx, access, pageID)
}

// restoreSelectedTab restores the selected tab when it is suspended and
// answers once it is; a tab closed meanwhile is no error. A tab whose page
// crashed comes back by its Reload only (ReloadCompanionPage), so selecting
// it does not load a page that may crash again.
func (m *Manager) restoreSelectedTab(ctx context.Context, access Access, pageID string) error {
	m.mu.Lock()
	rec := m.suspended[pageID]
	crashed := rec != nil && rec.failure != ""
	m.mu.Unlock()
	if rec == nil || crashed {
		return nil
	}
	_, _, err := m.resolvePage(ctx, access, pageID, false)
	if err != nil && errors.Is(err, errPageNotFound) && !m.hasTab(access.ThreadID, pageID) {
		return nil
	}
	return err
}
