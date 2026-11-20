package browser

import (
	"sort"
)

// A thread's tab set in tab-strip order: its live pages, its suspended
// records (suspend.go) and, for the companion, the tabs still being
// created (lifecycle.go).

// threadTab is one entry of a thread's tab set: a live page or a suspended
// one.
type threadTab struct {
	id      PageID
	info    PageInfo
	order   int64
	created int64
	lastUse int64
	live    *managedPage
	rec     *suspendedPage
	pending *pendingPage
}

// threadTabs is THE tab-strip order of a thread's pages, live and suspended:
// every surface that lists them (companion state, browser_pages, ambiguity
// errors, tab moves) uses it, so the UI, the tools and the errors never
// disagree about which tab is first. The companion also shows the tabs
// still being created and failed creations (companionTabsLocked); a tool
// sees a page once it exists.
func (m *Manager) threadTabs(tb *threadBrowser) []threadTab {
	m.mu.Lock()
	tabs := m.threadTabsLocked(tb)
	m.mu.Unlock()
	return tabs
}

// threadTabsLocked is threadTabs with m.mu held by the caller.
func (m *Manager) threadTabsLocked(tb *threadBrowser) []threadTab {
	return m.listTabsLocked(tb, false)
}

// companionTabsLocked is the tab set the companion shows: threadTabsLocked
// with the pending tabs, and each tab's companion-only state filled in
// (PageInfo.Creating, Restoring, Error, Loading, Favicon). The caller holds
// m.mu.
func (m *Manager) companionTabsLocked(tb *threadBrowser) []threadTab {
	return m.listTabsLocked(tb, true)
}

// listTabsLocked lists the tabs of the thread lifetime tb, none for nil.
func (m *Manager) listTabsLocked(tb *threadBrowser, companion bool) []threadTab {
	if tb == nil {
		return nil
	}
	var tabs []threadTab
	if companion {
		for _, pending := range m.pending {
			p := pending.p
			if p.owner != tb || pending.closed {
				continue
			}
			info := p.cachedInfo()
			info.Creating, info.Error = pending.err == "", pending.err
			tabs = append(tabs, threadTab{
				id: p.id, info: info, order: p.tabOrder.Load(), created: p.createdAt,
				lastUse: p.lastUse.Load(), pending: pending,
			})
		}
	}
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == tb {
				info := p.cachedInfo()
				if companion {
					info.Loading, info.Favicon = p.loadState()
					info.Agent = p.agentActive()
				}
				tabs = append(tabs, threadTab{
					id: p.id, info: info, order: p.tabOrder.Load(), created: p.createdAt,
					lastUse: p.lastUse.Load(), live: p,
				})
			}
		}
	}
	for _, rec := range m.suspended {
		if rec.owner == tb {
			info := rec.info()
			if companion {
				info.Restoring, info.Error = rec.restoring != nil, rec.failure
			}
			tabs = append(tabs, threadTab{
				id: rec.id, info: info, order: rec.tabOrder, created: rec.createdAt,
				lastUse: rec.lastUse, rec: rec,
			})
		}
	}
	sort.Slice(tabs, func(i, j int) bool {
		if tabs[i].order != tabs[j].order {
			return tabs[i].order < tabs[j].order
		}
		return tabs[i].created < tabs[j].created
	})
	return tabs
}

// hasTab reports whether pageID is one of the tabs tb's companion shows:
// live, suspended or pending.
func (m *Manager) hasTab(tb *threadBrowser, pageID PageID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasTabLocked(tb, pageID)
}

// livePageLocked finds a live page by id in any scope. The caller holds m.mu.
func (m *Manager) livePageLocked(pageID PageID) (*managedPage, *jarScope) {
	for _, scope := range m.scopes {
		if p := scope.pages[pageID]; p != nil {
			return p, scope
		}
	}
	return nil, nil
}
