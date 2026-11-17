package browser

import "strings"

// A thread's state push shows each of its tabs' info, loading state and
// favicon. Every write that changes one of them sets the page's unpushed
// record (pageLoad.unpushed, under metaMu), and only a push to every client
// clears it (buildThreadState), taking it before it reads the tabs, so a
// write that lands in between sets it again. publishPages pushes a thread
// whose pages hold one, checked and taken in one hold, so two publishers of
// one write push it once: a write is never lost whoever publishes next, and
// an operation that changed nothing pushes nothing.

// threadPagesLocked answers the thread's pages that have a managedPage: the
// ones being created and the live ones. The caller holds m.mu.
func (m *Manager) threadPagesLocked(threadID string) []*managedPage {
	var pages []*managedPage
	for _, pending := range m.pending {
		if pending.p.owner == threadID && !pending.closed {
			pages = append(pages, pending.p)
		}
	}
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == threadID {
				pages = append(pages, p)
			}
		}
	}
	return pages
}

// publishPages pushes the thread's state when a write to one of its pages
// is not in a push yet.
func (m *Manager) publishPages(threadID string) {
	if event, ok := m.buildThreadState(threadID, true, true); ok {
		if m.publishDecided != nil {
			m.publishDecided()
		}
		m.emit(event)
	}
}

// pageAdded follows p joining its thread's tabs (created, adopted or
// restored): its base, a use, the thread's active page when it has none, and
// a push. The base comes first, so a page shown at once shows on it.
func (m *Manager) pageAdded(p *managedPage) {
	m.applyPageBase(p)
	p.touch()
	m.ensureActivePage(p.owner, p.id)
	m.emitThreadState(p.owner)
	m.syncPanePresentation(p.owner)
}

// pageUsed follows an operation on p: a use, the thread's active page when it
// has none, and a push only when that or a write is not in a push yet.
func (m *Manager) pageUsed(p *managedPage) {
	p.touch()
	if m.ensureActivePage(p.owner, p.id) {
		m.emitThreadState(p.owner)
	} else {
		m.publishPages(p.owner)
	}
	m.syncPanePresentation(p.owner)
}

// threadState is the thread's state for one client's read (an attach, a
// call's answer). It leaves the unpushed records for the next push.
func (m *Manager) threadState(threadID string) CompanionEvent {
	event, _ := m.buildThreadState(threadID, false, false)
	return event
}

// buildThreadState builds the thread's state. For a push to every client
// (push) it first takes the unpushed records of the thread's pages; with
// ifUnpushed it builds nothing, answering false, when none holds one.
func (m *Manager) buildThreadState(threadID string, push, ifUnpushed bool) (CompanionEvent, bool) {
	m.mu.Lock()
	if push {
		unpushed := false
		for _, p := range m.threadPagesLocked(threadID) {
			p.metaMu.Lock()
			unpushed = unpushed || p.load.unpushed
			p.load.unpushed = false
			p.metaMu.Unlock()
		}
		if ifUnpushed && !unpushed {
			m.mu.Unlock()
			return CompanionEvent{}, false
		}
	}
	tabs := m.companionTabsLocked(threadID)
	if push && m.pushCopied != nil {
		m.pushCopied()
	}
	session, hasSession := m.sessions[threadID]
	active, _ := m.livePageLocked(session.ActivePageID)
	closedTabs := m.closedCountLocked(threadID)
	downloads := m.threadDownloadsLocked(threadID)
	m.stateSeq++
	seq := m.stateSeq
	recentGen := m.recentGen
	m.mu.Unlock()
	event := CompanionEvent{Kind: "state", ThreadID: threadID, Pages: make([]PageInfo, 0, len(tabs)), ClosedTabs: closedTabs, Downloads: downloads, Seq: seq}
	if active != nil && active.owner == threadID {
		event.FindMode = findModeOf(active)
	}
	visible := false
	if hasSession {
		visible = session.Visible
		event.SessionName = session.Name
	}
	event.Visible = &visible
	for _, tab := range tabs {
		event.Pages = append(event.Pages, tab.info)
	}
	event.ActivePageID = session.ActivePageID
	event.ViewportWidth, event.ViewportHeight = sessionViewport(session)
	event.ViewportSet = session.ViewportSet
	event.Jar = jarKindOf(session)
	event.RecentGen = recentGen
	return event, true
}

func (m *Manager) emit(event CompanionEvent) {
	m.mu.Lock()
	sink := m.eventSink
	m.mu.Unlock()
	if sink != nil {
		sink(event)
	}
}

// emitThreadState pushes the thread's state to every client.
func (m *Manager) emitThreadState(threadID string) {
	if strings.TrimSpace(threadID) != "" {
		event, _ := m.buildThreadState(threadID, true, false)
		m.emit(event)
	}
}

func (m *Manager) CompanionState(access Access) CompanionEvent {
	return m.threadState(access.ThreadID)
}
