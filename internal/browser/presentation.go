package browser

import "strings"

// syncPanePresentation decides WHICH of a thread's pages is presented and
// whether the pane is showing at all, then tells the engine the outcome. The
// decision itself stays here, in the Manager: an engine is told the outcome,
// never the rule.
//
// Where the pane SITS is the frontend's answer: the mounted pane reports its
// host rect (SetPaneRect) and that rect rides along, so bounds always land
// before the show. At most one pane per thread exists (a frontend invariant),
// and a mounted pane without a usable rect yet keeps the view hidden rather
// than flashing it at a stale place.
//
// An engine whose pages are not real windows implements no paneHost, so this
// is a type assertion that fails and costs nothing on every deployment that
// cannot present a page at all.
//
// A find runs on the shown active page only, so this is also where a find on
// a page that stopped being it (a tab switch, a hidden companion) ends, and
// so does a find bar whose pane mount is gone.
// Occlusion is not hiding: an overlay over the pane leaves the find running.
//
// Passes of a thread never overlap: each reads the state and tells the
// engine without a lock held, so two at once could land in either order and
// leave a page shown that the newer state hides. A call made while a pass
// runs leaves it to that caller, which runs one more pass after its own, so
// the last pass always reads the newest state and no caller waits on a pass
// it did not start (an engine's UI thread among them).
func (m *Manager) syncPanePresentation(threadID string) {
	m.mu.Lock()
	st := m.presenting[threadID]
	if st == nil {
		if m.presenting == nil {
			m.presenting = make(map[string]*presentSync)
		}
		st = &presentSync{}
		m.presenting[threadID] = st
	}
	st.dirty = true
	if st.busy {
		m.mu.Unlock()
		return
	}
	st.busy = true
	for st.dirty {
		st.dirty = false
		m.mu.Unlock()
		m.presentPanes(threadID)
		m.mu.Lock()
	}
	delete(m.presenting, threadID)
	m.mu.Unlock()
}

// presentSync is one thread's presentation passes: busy while one runs,
// dirty when another was asked for. Guarded by Manager.mu.
type presentSync struct{ busy, dirty bool }

// presentedPage is the page a presentation pass showed, and whether it
// painted the document base under it (pageBase).
type presentedPage struct {
	id       string
	document bool
}

// documentBase is the base color under a page that shows a document: the
// white every browser paints where a document leaves its own background
// transparent. A page whose address carries no document (a blank new tab)
// shows the pane's own color instead.
const documentBase = "#ffffff"

// pageShowsDocument answers whether a page at url shows a document. A blank
// new tab, and a page the engine has not given an address yet, show none.
func pageShowsDocument(url string) bool {
	switch {
	case url == "", url == "about:blank":
		return false
	case strings.HasPrefix(url, "about:blank#"), strings.HasPrefix(url, "about:blank?"):
		return false
	}
	return true
}

// pageBase is the color an engine paints under a presented page: the
// document base when the page shows a document, and the pane's own color
// (the host rect's Background) when it shows none, so an empty tab is the
// pane and a page without a background of its own reads as it does in any
// browser. The decision is the Manager's because only it knows the page's
// address; a host paints what it is told.
func pageBase(document bool, pane string) string {
	if document {
		return documentBase
	}
	return pane
}

// presentAddressCommitted follows a committed change of p's address: when p
// is the page its thread's pane shows and the address changed whether it
// shows a document, the pane is presented again so the engine paints the
// base the new address calls for. A pass under way when the address was
// committed may have read the old one, so it is asked for another.
func (m *Manager) presentAddressCommitted(p *managedPage) {
	document := pageShowsDocument(p.cachedInfo().URL)
	m.mu.Lock()
	shown, ok := m.presented[p.owner]
	stale := m.presenting[p.owner] != nil || ok && shown.id == p.id && shown.document != document
	m.mu.Unlock()
	if stale {
		m.syncPanePresentation(p.owner)
	}
}

// presentPanes is one presentation pass of syncPanePresentation.
//
// The pass hides every page but the one the thread had selected when it
// began (its candidate), then decides, in one hold of Manager.mu, whether to
// show the candidate against the selection, the presenter and the viewport
// as they are then: a candidate that lost the selection meanwhile (a switch
// away and back while it was laid out, a restore that finished after the
// user moved on) is hidden, never shown, and the switch's own request runs
// the next pass. Only the engine calls after that hold are outside it: a
// selection made during them is newer than the show and its pass hides it.
//
// A page is never newly shown while it lags the thread's viewport (a pane
// resize lays out only the page shown then, viewport.go): it stays hidden and
// the drain lays it out and runs another pass. A page already shown stays
// shown through its own resize, whose drain is under way, and a page that
// refused the size is shown anyway, under the error that refusal raised.
func (m *Manager) presentPanes(threadID string) {
	host, ok := m.engine.(paneHost)
	m.mu.Lock()
	candidateID := m.sessions[threadID].ActivePageID
	var pages []*managedPage
	var candidate *managedPage
	var closing []*managedPage
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner != threadID {
				continue
			}
			pages = append(pages, p)
			if p.id == candidateID {
				candidate = p
			}
		}
		for _, c := range scope.closing {
			if c.p.owner == threadID {
				closing = append(closing, c.p)
			}
		}
	}
	m.mu.Unlock()
	m.endHiddenFinds(pages)
	if !ok {
		return
	}
	// A closed page is hidden at once; its engine teardown may run a while.
	// Hiding a page whose driver has since closed is a no-op on every host.
	for _, p := range closing {
		host.HidePage(p.driver.Handle())
	}
	for _, p := range pages {
		if p != candidate {
			host.HidePage(p.driver.Handle())
		}
	}
	if m.presentDeciding != nil {
		m.presentDeciding(threadID, candidate)
	}
	show, placement, lagging := m.decidePresentation(threadID, candidate)
	document := false
	if candidate != nil {
		if show {
			document = pageShowsDocument(candidate.cachedInfo().URL)
			placement.Rect.Background = pageBase(document, placement.Rect.Background)
			host.SetPageBounds(candidate.driver.Handle(), placement)
			host.ShowPage(candidate.driver.Handle())
		} else {
			host.HidePage(candidate.driver.Handle())
		}
	}
	m.mu.Lock()
	if m.presented == nil {
		m.presented = make(map[string]presentedPage)
	}
	if show {
		m.presented[threadID] = presentedPage{id: candidate.id, document: document}
	} else {
		delete(m.presented, threadID)
	}
	m.mu.Unlock()
	if lagging {
		m.scheduleViewport(threadID)
	}
	// A focus intent waits for its page's presentation, never ShowPage.
	m.applyFocusIntent(threadID)
}

// decidePresentation is presentPanes' one decision, in one hold of
// Manager.mu: whether the pass shows its candidate, where, and whether the
// candidate waits for its layout. A candidate that is no longer the
// thread's selected live page is not shown. A lagging candidate is recorded
// as awaiting its layout in the same hold, so the apply that lays it out
// either is seen here or finds the record (presentLaidOut).
func (m *Manager) decidePresentation(threadID string, candidate *managedPage) (show bool, placement PanePlacement, lagging bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.awaitingLayout, threadID)
	session, hasSession := m.sessions[threadID]
	mount, rect, shown := m.presenterLocked(threadID)
	if candidate == nil || !hasSession || !session.Visible || !shown || session.ActivePageID != candidate.id {
		return false, PanePlacement{}, false
	}
	if live, _ := m.livePageLocked(candidate.id); live != candidate {
		return false, PanePlacement{}, false
	}
	pageW, pageH := sessionViewport(session)
	// The page keeps the session's viewport; the pane shows it scaled to
	// fit. A pane too small or too occluded to show any of it presents
	// nothing rather than a sliver.
	placement, show = placePage(rect, pageW, pageH)
	placement.Mount = mount
	if !show {
		return false, PanePlacement{}, false
	}
	if candidate.id != m.presented[threadID].id {
		want := packViewport(pageW, pageH)
		if candidate.viewport.Load() != want && candidate.viewportFailed.Load() != want {
			if m.awaitingLayout == nil {
				m.awaitingLayout = make(map[string]string)
			}
			m.awaitingLayout[threadID] = candidate.id
			return false, PanePlacement{}, true
		}
	}
	return true, placement, false
}
