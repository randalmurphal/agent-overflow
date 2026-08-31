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
	p, _, err := m.lookupOwnedPage(access, strings.TrimSpace(pageID))
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
	m.syncThreadStream(access.ThreadID)
	return info, nil
}

func (m *Manager) LabelPage(_ context.Context, access Access, pageID, label string) (PageInfo, error) {
	p, _, err := m.lookupOwnedPage(access, strings.TrimSpace(pageID))
	if err != nil {
		return PageInfo{}, err
	}
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) > 80 {
		return PageInfo{}, fmt.Errorf("browser: page label exceeds 80 characters")
	}
	if strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return PageInfo{}, fmt.Errorf("browser: page label cannot contain control characters")
	}
	m.mu.Lock()
	for _, scope := range m.scopes {
		for _, candidate := range scope.pages {
			if candidate.owner == access.ThreadID && candidate.id != p.id && label != "" && strings.EqualFold(candidate.cachedInfo().Label, label) {
				m.mu.Unlock()
				return PageInfo{}, fmt.Errorf("browser: page label %q is already used by page %s", label, candidate.id)
			}
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

func (m *Manager) Visibility(_ context.Context, access Access, visible *bool, pageID string) (SessionInfo, error) {
	pageID = strings.TrimSpace(pageID)
	if visible != nil && *visible {
		if pageID != "" {
			if _, _, err := m.lookupOwnedPage(access, pageID); err != nil {
				return SessionInfo{}, err
			}
		} else {
			pages := m.ownedPages(access.ThreadID)
			switch len(pages) {
			case 0:
				return SessionInfo{}, fmt.Errorf("browser: cannot show the companion because this thread has no open pages")
			case 1:
				pageID = pages[0].id
			default:
				return SessionInfo{}, ambiguousPageError(pages)
			}
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
		if info.Visible {
			m.syncThreadStream(access.ThreadID)
		} else {
			for _, p := range m.ownedPages(access.ThreadID) {
				p.stopStream()
			}
		}
	}
	return info, nil
}

func (m *Manager) Viewport(_ context.Context, access Access, opts ViewportOptions) (SessionInfo, error) {
	m.mu.Lock()
	info := m.sessionLocked(access.ThreadID)
	switch strings.ToLower(strings.TrimSpace(opts.Action)) {
	case "get", "":
	case "reset":
		info.ViewportW, info.ViewportH = defaultViewportWidth, defaultViewportHeight
		info.ViewportSet = false
	case "set":
		if opts.Width < minCompanionWidth || opts.Width > maxCompanionWidth || opts.Height < minCompanionHeight || opts.Height > maxCompanionHeight {
			m.mu.Unlock()
			return SessionInfo{}, fmt.Errorf("browser: viewport must be between %dx%d and %dx%d", minCompanionWidth, minCompanionHeight, maxCompanionWidth, maxCompanionHeight)
		}
		info.ViewportW, info.ViewportH = opts.Width, opts.Height
		info.ViewportSet = true
	default:
		m.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("browser: viewport action must be get, set, or reset")
	}
	info.UpdatedAt = time.Now()
	m.sessions[access.ThreadID] = info
	m.mu.Unlock()
	if opts.Action != "get" && opts.Action != "" {
		for _, p := range m.ownedPages(access.ThreadID) {
			p.stopStream()
			p.streamCmdMu.Lock()
			opCtx, cancel := operationContext(context.Background(), p.ctx, 5*time.Second)
			var err error
			if info.ViewportSet {
				err = p.driver.SetViewport(opCtx, info.ViewportW, info.ViewportH)
			} else {
				err = p.driver.ClearViewport(opCtx)
			}
			cancel()
			p.streamCmdMu.Unlock()
			if err != nil {
				return SessionInfo{}, fmt.Errorf("browser: apply viewport: %w", err)
			}
		}
		m.syncThreadStream(access.ThreadID)
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

func (m *Manager) repairActivePage(threadID string) {
	m.mu.Lock()
	info := m.sessionLocked(threadID)
	var replacement *managedPage
	activeExists := false
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner != threadID {
				continue
			}
			if p.id == info.ActivePageID {
				activeExists = true
			}
			if replacement == nil || p.lastUse.Load() > replacement.lastUse.Load() {
				replacement = p
			}
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

func (m *Manager) applyConfiguredViewport(p *managedPage) error {
	m.mu.Lock()
	info, ok := m.sessions[p.owner]
	m.mu.Unlock()
	if !ok || !info.ViewportSet {
		return nil
	}
	ctx, cancel := operationContext(context.Background(), p.ctx, 5*time.Second)
	defer cancel()
	if err := p.driver.SetViewport(ctx, info.ViewportW, info.ViewportH); err != nil {
		return fmt.Errorf("browser: apply viewport: %w", err)
	}
	return nil
}
