package browser

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"agent-overflow/internal/browser/turnorder"
)

// SelectPage pins one of the caller's tabs as the companion's. The pane
// presents it first; a suspended page then restores, a page that lags the
// pane takes its size and is shown, and its info is read.
func (m *Manager) SelectPage(ctx context.Context, access Access, pageID PageID) (PageInfo, error) {
	if err := access.bound(); err != nil {
		return PageInfo{}, err
	}
	pageID = pageID.trim()
	// An agent's selection keeps arrival order (selection.go).
	if _, err := m.selectTab(access, turnorder.Unordered(), pageID); err != nil {
		return PageInfo{}, err
	}
	p, _, err := m.resolvePage(ctx, access, pageID, false)
	if err != nil {
		return PageInfo{}, err
	}
	// The page takes the thread viewport before its info is read: a page
	// that lagged the pane is shown as soon as that layout lands
	// (presentLaidOut), not after the read, which the drain would otherwise
	// wait behind for the page lock.
	if err := m.lockPageForOperation(ctx, p); err != nil {
		return PageInfo{}, err
	}
	opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
	info, _, infoErr := m.readInfo(opCtx, p, pageListing{})
	cancel()
	p.mu.Unlock()
	if infoErr != nil {
		return PageInfo{}, infoErr
	}
	m.publishPages(access.ThreadID())
	return info, nil
}

// selectTab makes one of the thread's tabs, in any state, the active one and
// presents it before any page work: a live page is shown at once, and a tab
// with no page yet (creating, restoring, suspended, failed) shows its state
// until it has one. A live page counts as used. turn is the selection's
// place in the user's order (selection.go), which its hold commits. It
// answers the tab it selected, the only one its caller may restore, and ""
// when turn was passed and selected nothing.
func (m *Manager) selectTab(access Access, turn *turnorder.Turn, pageID PageID) (PageID, error) {
	return m.selectTabWhere(access, turn, func(tabs []threadTab, _ PageID) int {
		for i := range tabs {
			if tabs[i].id == pageID {
				return i
			}
		}
		return -1
	})
}

// selectTabWhere is selectTab of the tab pick chooses from the companion's
// tabs in strip order and the active tab's id, under one hold of m.mu that
// commits turn, so the choice and the selection see the same tabs. A pick
// of -1 is errPageNotFound. The hold waits for a jar switch of the thread
// writing its saved copy, which holds the active tab.
func (m *Manager) selectTabWhere(access Access, turn *turnorder.Turn, pick func(tabs []threadTab, activePageID PageID) int) (PageID, error) {
	defer turn.Release()
	if err := m.lockStructure(context.Background(), access.tb); err != nil {
		return "", err
	}
	if err := access.admitLocked(); err != nil {
		m.mu.Unlock()
		return "", err
	}
	if turn.Commit() == turnorder.Passed {
		// A repeat, or the pane it came from is gone.
		m.mu.Unlock()
		return "", nil
	}
	tb := access.tb
	tabs := m.companionTabsLocked(tb)
	at := pick(tabs, tb.session.activePage())
	if at < 0 || at >= len(tabs) {
		m.mu.Unlock()
		return "", errPageNotFound
	}
	found := &tabs[at]
	pageID := found.id
	tb.session.ActivePageID = string(pageID)
	tb.session.UpdatedAt = time.Now()
	m.mu.Unlock()
	if found.live != nil {
		found.live.touch()
	}
	m.emitThreadState(access.ThreadID())
	m.syncPanePresentation(access.ThreadID())
	return pageID, nil
}

// labelHolderLocked answers tb's other tab (not pageID) whose label equals
// label, ignoring case, or "" when none does: a thread's labels are unique,
// and every label write checks here in the m.mu hold that commits it. An
// empty label is never held. The caller holds m.mu.
func (m *Manager) labelHolderLocked(tb *threadBrowser, pageID PageID, label string) PageID {
	if label == "" {
		return ""
	}
	for _, tab := range m.threadTabsLocked(tb) {
		if tab.id != pageID && strings.EqualFold(tab.info.Label, label) {
			return tab.id
		}
	}
	return ""
}

func (m *Manager) LabelPage(ctx context.Context, access Access, pageID PageID, label string) (PageInfo, error) {
	if err := access.bound(); err != nil {
		return PageInfo{}, err
	}
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) > maxPageLabelRunes {
		return PageInfo{}, fmt.Errorf("browser: page label exceeds 80 characters")
	}
	if strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return PageInfo{}, fmt.Errorf("browser: page label cannot contain control characters")
	}
	p, _, err := m.resolvePage(ctx, access, pageID, false)
	if err != nil {
		return PageInfo{}, err
	}
	// A label is in the saved copy a jar switch writes.
	if err := m.lockStructure(ctx, access.tb); err != nil {
		return PageInfo{}, err
	}
	if holder := m.labelHolderLocked(access.tb, p.id, label); holder != "" {
		m.mu.Unlock()
		return PageInfo{}, fmt.Errorf("browser: page label %q is already used by page %s", label, holder)
	}
	info, _ := p.commitInfo(pageUpdate{Label: &label})
	m.mu.Unlock()
	m.emitThreadState(access.ThreadID())
	return info, nil
}

func (m *Manager) NameSession(_ context.Context, access Access, name string) (SessionInfo, error) {
	if err := access.bound(); err != nil {
		return SessionInfo{}, err
	}
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 120 {
		return SessionInfo{}, fmt.Errorf("browser: session name exceeds 120 characters")
	}
	m.mu.Lock()
	if err := access.admitLocked(); err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}
	access.tb.session.Name = name
	access.tb.session.UpdatedAt = time.Now()
	info := access.tb.session
	m.mu.Unlock()
	m.emitThreadState(access.ThreadID())
	return info, nil
}

// Visibility shows the companion on one page. A suspended page is restored
// by the pane that presents it (BrowserPane), not here, so a page that
// cannot be restored still opens the companion, where its refusal shows and
// the user can navigate or close it. Hiding touches no page.
func (m *Manager) Visibility(ctx context.Context, access Access, visible *bool, pageID PageID) (SessionInfo, error) {
	if err := access.bound(); err != nil {
		return SessionInfo{}, err
	}
	pageID = pageID.trim()
	if visible != nil && *visible {
		if pageID == "" {
			tabs := m.threadTabs(access.tb)
			switch len(tabs) {
			case 0:
				return SessionInfo{}, fmt.Errorf("browser: cannot show the companion because this thread has no open pages")
			case 1:
				pageID = tabs[0].id
			default:
				return SessionInfo{}, ambiguousPageError(tabs)
			}
		}
		if !m.hasTab(access.tb, pageID) {
			return SessionInfo{}, errPageNotFound
		}
	} else if pageID != "" {
		return SessionInfo{}, fmt.Errorf("browser: page_id is only valid when visible is true")
	}
	if err := m.lockStructure(ctx, access.tb); err != nil {
		return SessionInfo{}, err
	}
	if err := access.admitLocked(); err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}
	tb := access.tb
	if visible != nil {
		tb.session.Visible = *visible
		if *visible {
			tb.session.ActivePageID = string(pageID)
		}
		tb.session.UpdatedAt = time.Now()
	}
	info := tb.session
	m.mu.Unlock()
	if visible != nil {
		m.emitThreadState(access.ThreadID())
		m.syncPanePresentation(access.ThreadID())
	}
	return info, nil
}

// Pages lists the thread's pages, suspended ones included as they were saved:
// listing is not a touch, so it restores nothing.
func (m *Manager) Pages(ctx context.Context, access Access) ([]PageInfo, error) {
	if err := access.bound(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	tabs := m.threadTabsLocked(access.tb)
	var activePageID PageID
	if access.tb != nil {
		activePageID = access.tb.session.activePage()
	}
	m.mu.Unlock()
	out := make([]PageInfo, 0, len(tabs))
	for _, tab := range tabs {
		p := tab.live
		if p == nil {
			info := tab.info
			info.Selected = PageID(info.ID) == activePageID
			info.LastOpened = time.Unix(0, tab.lastUse).UTC().Format(time.RFC3339Nano)
			out = append(out, info)
			continue
		}
		p.mu.Lock()
		opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
		listing := pageListing{Selected: p.id == activePageID, LastOpened: time.Unix(0, p.lastUse.Load()).UTC().Format(time.RFC3339Nano)}
		info, _, err := m.readInfo(opCtx, p, listing)
		cancel()
		p.mu.Unlock()
		if err == nil {
			info.Selected, info.LastOpened = listing.Selected, listing.LastOpened
			out = append(out, info)
		}
	}
	// What the reads found, the tabs show.
	m.publishPages(access.ThreadID())
	sort.Slice(out, func(i, j int) bool { return out[i].LastOpened > out[j].LastOpened })
	return out, nil
}

// ensureActivePage makes pageID tb's active page when it has none, and
// reports whether it did. A selection is a structural change: it waits out
// a jar switch writing the active tab (lockStructure).
func (m *Manager) ensureActivePage(tb *threadBrowser, pageID PageID) bool {
	_ = m.lockStructure(context.Background(), tb)
	defer m.mu.Unlock()
	if tb.session.activePage() != "" {
		return false
	}
	tb.session.ActivePageID = string(pageID)
	tb.session.UpdatedAt = time.Now()
	return true
}

// repairActivePage keeps tb's active page one of its tabs, live, suspended
// or pending: a missing one is replaced by the most recently used tab
// (recentTabLocked), and a lifetime with none hides its companion. It waits
// out a jar switch writing the active tab (lockStructure).
func (m *Manager) repairActivePage(tb *threadBrowser) {
	_ = m.lockStructure(context.Background(), tb)
	m.repairActivePageLocked(tb)
	m.mu.Unlock()
}

// repairActivePageLocked is repairActivePage's hold. The caller holds m.mu.
func (m *Manager) repairActivePageLocked(tb *threadBrowser) {
	tabs := m.companionTabsLocked(tb)
	for _, tab := range tabs {
		if tab.id == tb.session.activePage() {
			return
		}
	}
	tb.session.ActivePageID = string(recentTabLocked(tabs))
	if tb.session.activePage() == "" {
		tb.session.Visible = false
	}
	tb.session.UpdatedAt = time.Now()
}

// recentTabLocked is the most recently used of tabs, "" for none: the tab
// a thread with no active tab shows. The caller holds m.mu.
func recentTabLocked(tabs []threadTab) PageID {
	var recent *threadTab
	for i := range tabs {
		if recent == nil || tabs[i].lastUse > recent.lastUse {
			recent = &tabs[i]
		}
	}
	if recent == nil {
		return ""
	}
	return recent.id
}
