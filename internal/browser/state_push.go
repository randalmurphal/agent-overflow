package browser

// A thread's state push shows each of its tabs' info, loading state and
// favicon. Every write that changes one of them sets the page's unpushed
// record (pageLoad.unpushed, under metaMu), and only a push to every client
// clears it (buildThreadState), taking it before it reads the tabs, so a
// write that lands in between sets it again. publishPages pushes a thread
// whose pages hold one, checked and taken in one hold, so two publishers of
// one write push it once: a write is never lost whoever publishes next, and
// an operation that changed nothing pushes nothing.

// threadPagesLocked answers tb's pages that have a managedPage: the ones
// being created and the live ones; none for nil. The caller holds m.mu.
func (m *Manager) threadPagesLocked(tb *threadBrowser) []*managedPage {
	if tb == nil {
		return nil
	}
	var pages []*managedPage
	for _, pending := range m.pending {
		if pending.p.owner == tb && !pending.closed {
			pages = append(pages, pending.p)
		}
	}
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == tb {
				pages = append(pages, p)
			}
		}
	}
	return pages
}

// publishPages pushes the thread's state when a write to one of its pages
// is not in a push yet.
func (m *Manager) publishPages(threadID ThreadID) {
	if event, ok := m.buildThreadState(threadID, true, true); ok {
		if m.publishDecided != nil {
			m.publishDecided()
		}
		m.emit(event)
	}
}

// pageAdded follows p joining its thread's tabs (created, adopted or
// restored), whose hold selected it when its thread had no active tab
// (listPageLocked): its base, a use and a push. The base comes first, so a
// page shown at once shows on it.
func (m *Manager) pageAdded(p *managedPage) {
	m.applyPageBase(p)
	p.touch()
	m.emitThreadState(p.owner.id)
	m.syncPanePresentation(p.owner.id)
}

// pageUsed follows an operation on p: a use, the thread's active page when it
// has none, and a push only when that or a write is not in a push yet.
func (m *Manager) pageUsed(p *managedPage) {
	p.touch()
	if m.ensureActivePage(p.owner, p.id) {
		m.emitThreadState(p.owner.id)
	} else {
		m.publishPages(p.owner.id)
	}
	m.syncPanePresentation(p.owner.id)
}

// threadState is the thread's state for one client's read (an attach, a
// call's answer). It leaves the unpushed records for the next push.
func (m *Manager) threadState(threadID ThreadID) CompanionEvent {
	event, _ := m.buildThreadState(threadID, false, false)
	return event
}

// buildThreadState builds the thread's state, its current lifetime's (an
// empty one when it has none). For a push to every client (push) it first
// takes the unpushed records of the thread's pages; with ifUnpushed it
// builds nothing, answering false, when none holds one.
func (m *Manager) buildThreadState(threadID ThreadID, push, ifUnpushed bool) (CompanionEvent, bool) {
	m.mu.Lock()
	tb := m.currentLocked(threadID)
	if push {
		unpushed := false
		for _, p := range m.threadPagesLocked(tb) {
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
	tabs := m.companionTabsLocked(tb)
	if push && m.pushCopied != nil {
		m.pushCopied()
	}
	session := newThreadBrowser(threadID).session
	if tb != nil {
		session = tb.session
	}
	active, _ := m.livePageLocked(session.activePage())
	closedTabs := closedCountLocked(tb)
	downloads, downloadsGen := m.threadDownloadsLocked(tb)
	m.stateSeq++
	seq := m.stateSeq
	recentGen := m.recentGenTokenLocked()
	m.mu.Unlock()
	event := CompanionEvent{Kind: "state", ThreadID: string(threadID), Pages: make([]PageInfo, 0, len(tabs)), ClosedTabs: closedTabs, Downloads: downloads, DownloadsGen: downloadsGen, Seq: seq}
	if active != nil && tb != nil && active.owner == tb {
		event.FindMode = findModeOf(active)
	}
	visible := session.Visible
	event.SessionName = session.Name
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
func (m *Manager) emitThreadState(threadID ThreadID) {
	if threadID.trim() != "" {
		event, _ := m.buildThreadState(threadID, true, false)
		m.emit(event)
	}
}

// CompanionState answers the state of caller's thread as it is now.
func (m *Manager) CompanionState(caller Caller) CompanionEvent {
	return m.threadState(caller.ThreadID)
}
