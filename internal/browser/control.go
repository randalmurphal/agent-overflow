package browser

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (m *Manager) SelectPage(ctx context.Context, access Access, pageID string) (PageInfo, error) {
	p, _, err := m.resolvePage(ctx, access, pageID, false)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
	info, infoErr := m.pageInfo(opCtx, p)
	cancel()
	p.mu.Unlock()
	if infoErr != nil {
		return PageInfo{}, infoErr
	}
	p.setInfo(info)
	info = p.cachedInfo()
	p.touch()
	m.setActivePage(access.ThreadID, p.id)
	m.emitThreadState(access.ThreadID)
	m.syncPanePresentation(access.ThreadID)
	return info, nil
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
		info = SessionInfo{Visible: false, ViewportW: defaultViewportWidth, ViewportH: defaultViewportHeight}
	}
	return info
}

func (m *Manager) ensureActivePage(threadID, pageID string) {
	m.mu.Lock()
	info := m.sessionLocked(threadID)
	if info.ActivePageID == "" {
		info.ActivePageID = pageID
		info.UpdatedAt = time.Now()
		m.sessions[threadID] = info
	}
	m.mu.Unlock()
}

func (m *Manager) setActivePage(threadID, pageID string) {
	m.mu.Lock()
	info := m.sessionLocked(threadID)
	info.ActivePageID = pageID
	info.UpdatedAt = time.Now()
	m.sessions[threadID] = info
	m.mu.Unlock()
}

// repairActivePage keeps the thread's active page one of its pages, live or
// suspended: a missing one is replaced by the most recently used page, and a
// thread with none hides its companion.
func (m *Manager) repairActivePage(threadID string) {
	m.mu.Lock()
	info := m.sessionLocked(threadID)
	var replacement *threadTab
	activeExists := false
	tabs := m.threadTabsLocked(threadID)
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
