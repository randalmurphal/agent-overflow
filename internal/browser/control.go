package browser

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SelectPage pins one of the caller's tabs as the companion's. The pane
// presents it first; a suspended page then restores, a page that lags the
// pane takes its size and is shown, and its info is read.
func (m *Manager) SelectPage(ctx context.Context, access Access, pageID string) (PageInfo, error) {
	pageID = strings.TrimSpace(pageID)
	if err := m.selectTab(access, pageID); err != nil {
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
	info, _, infoErr := m.refreshInfo(opCtx, p)
	cancel()
	p.mu.Unlock()
	if infoErr != nil {
		return PageInfo{}, infoErr
	}
	return info, nil
}

// selectTab makes one of the thread's tabs, in any state, the active one and
// presents it before any page work: a live page is shown at once, and a tab
// with no page yet (creating, restoring, suspended, failed) shows its state
// until it has one. A live page counts as used.
func (m *Manager) selectTab(access Access, pageID string) error {
	_, err := m.selectTabWhere(access, func(tabs []threadTab, _ string) int {
		for i := range tabs {
			if tabs[i].id == pageID {
				return i
			}
		}
		return -1
	})
	return err
}

// selectTabWhere is selectTab of the tab pick chooses from the companion's
// tabs in strip order and the active tab's id, under one hold of m.mu so the
// choice and the selection see the same tabs. A pick of -1 is
// errPageNotFound. It answers the selected id.
func (m *Manager) selectTabWhere(access Access, pick func(tabs []threadTab, activePageID string) int) (string, error) {
	m.mu.Lock()
	tabs := m.companionTabsLocked(access.ThreadID)
	info := m.sessionLocked(access.ThreadID)
	at := pick(tabs, info.ActivePageID)
	if at < 0 || at >= len(tabs) {
		m.mu.Unlock()
		return "", errPageNotFound
	}
	found := &tabs[at]
	pageID := found.id
	info.ActivePageID = pageID
	info.UpdatedAt = time.Now()
	m.sessions[access.ThreadID] = info
	m.mu.Unlock()
	if found.live != nil {
		found.live.touch()
	}
	m.emitThreadState(access.ThreadID)
	m.syncPanePresentation(access.ThreadID)
	return pageID, nil
}

func (m *Manager) LabelPage(ctx context.Context, access Access, pageID, label string) (PageInfo, error) {
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
	m.mu.Lock()
	for _, tab := range m.threadTabsLocked(access.ThreadID) {
		if tab.id != p.id && label != "" && strings.EqualFold(tab.info.Label, label) {
			m.mu.Unlock()
			return PageInfo{}, fmt.Errorf("browser: page label %q is already used by page %s", label, tab.id)
		}
	}
	info := p.setLabel(label)
	m.mu.Unlock()
	m.emitThreadState(access.ThreadID)
	return info, nil
}

func (m *Manager) NameSession(_ context.Context, access Access, name string) (SessionInfo, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 120 {
		return SessionInfo{}, fmt.Errorf("browser: session name exceeds 120 characters")
	}
	m.mu.Lock()
	info := m.sessionLocked(access.ThreadID)
	info.Name = name
	info.UpdatedAt = time.Now()
	m.sessions[access.ThreadID] = info
	m.mu.Unlock()
	m.emitThreadState(access.ThreadID)
	return info, nil
}

// Visibility shows the companion on one page. A suspended page is restored
// by the pane that presents it (BrowserPane), not here, so a page that
// cannot be restored still opens the companion, where its refusal shows and
// the user can navigate or close it. Hiding touches no page.
func (m *Manager) Visibility(ctx context.Context, access Access, visible *bool, pageID string) (SessionInfo, error) {
	pageID = strings.TrimSpace(pageID)
	if visible != nil && *visible {
		if pageID == "" {
			tabs := m.threadTabs(access.ThreadID)
			switch len(tabs) {
			case 0:
				return SessionInfo{}, fmt.Errorf("browser: cannot show the companion because this thread has no open pages")
			case 1:
				pageID = tabs[0].id
			default:
				return SessionInfo{}, ambiguousPageError(tabs)
			}
		}
		if !m.hasTab(access.ThreadID, pageID) {
			return SessionInfo{}, errPageNotFound
		}
	} else if pageID != "" {
		return SessionInfo{}, fmt.Errorf("browser: page_id is only valid when visible is true")
	}
	m.mu.Lock()
	info := m.sessionLocked(access.ThreadID)
	if visible != nil {
		info.Visible = *visible
		if *visible {
			info.ActivePageID = pageID
		}
		info.UpdatedAt = time.Now()
		m.sessions[access.ThreadID] = info
	}
	m.mu.Unlock()
	if visible != nil {
		m.emitThreadState(access.ThreadID)
		m.syncPanePresentation(access.ThreadID)
	}
	return info, nil
}

func (m *Manager) sessionLocked(threadID string) SessionInfo {
	info, ok := m.sessions[threadID]
	if !ok {
		info = SessionInfo{Visible: false, ViewportW: defaultViewportWidth, ViewportH: defaultViewportHeight, Jar: JarWorktree}
	}
	return info
}

// ensureActivePage makes pageID the thread's active page when it has none,
// and reports whether it did.
func (m *Manager) ensureActivePage(threadID, pageID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	info := m.sessionLocked(threadID)
	if info.ActivePageID != "" {
		return false
	}
	info.ActivePageID = pageID
	info.UpdatedAt = time.Now()
	m.sessions[threadID] = info
	return true
}

func (m *Manager) setActivePage(threadID, pageID string) {
	m.mu.Lock()
	info := m.sessionLocked(threadID)
	info.ActivePageID = pageID
	info.UpdatedAt = time.Now()
	m.sessions[threadID] = info
	m.mu.Unlock()
}

// repairActivePage keeps the thread's active page one of its tabs, live,
// suspended or pending: a missing one is replaced by the most recently used
// tab, and a thread with none hides its companion.
func (m *Manager) repairActivePage(threadID string) {
	m.mu.Lock()
	info := m.sessionLocked(threadID)
	var replacement *threadTab
	activeExists := false
	tabs := m.companionTabsLocked(threadID)
	for i := range tabs {
		if tabs[i].id == info.ActivePageID {
			activeExists = true
		}
		if replacement == nil || tabs[i].lastUse > replacement.lastUse {
			replacement = &tabs[i]
		}
	}
	if !activeExists {
		info.ActivePageID = ""
		if replacement != nil {
			info.ActivePageID = replacement.id
		} else {
			info.Visible = false
		}
		info.UpdatedAt = time.Now()
		m.sessions[threadID] = info
	}
	m.mu.Unlock()
}
