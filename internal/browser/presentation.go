package browser

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
//
// A page is never shown on a base older than its inputs: the pass applies
// the candidate's base (applyPageBase) before its bounds and its show, so a
// show waits for a base application already under way and then carries
// whatever the address and the pane's color call for at that moment. No
// Manager.mu is held there, and nothing holding a page's baseMu runs a pass.
func (m *Manager) presentPanes(threadID string) {
	host, ok := m.engine.(paneHost)
	m.mu.Lock()
	// The candidate is the current lifetime's selection; every other page
	// of the thread, an earlier lifetime's still closing included, is
	// hidden.
	current := m.currentLocked(threadID)
	candidateID := m.currentSessionLocked(threadID).ActivePageID
	var pages []*managedPage
	var candidate *managedPage
	var closing []*managedPage
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner.id != threadID {
				continue
			}
			pages = append(pages, p)
			if p.owner == current && p.id == candidateID {
				candidate = p
			}
		}
		for _, c := range scope.closing {
			if c.p.owner.id == threadID {
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
	if candidate != nil {
		if show {
			// The base the inputs call for now lands first: an application
			// another goroutine has under way finishes before this one reads.
			m.applyPageBase(candidate)
			host.SetPageBounds(candidate.driver.Handle(), placement)
			host.ShowPage(candidate.driver.Handle())
		} else {
			host.HidePage(candidate.driver.Handle())
		}
	}
	m.mu.Lock()
	if m.presented == nil {
		m.presented = make(map[string]string)
	}
	if show {
		m.presented[threadID] = candidate.id
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
	tb := m.currentLocked(threadID)
	if candidate == nil || tb == nil || candidate.owner != tb {
		return false, PanePlacement{}, false
	}
	session := tb.session
	mount, rect, shown := m.presenterLocked(tb)
	if !session.Visible || !shown || session.ActivePageID != candidate.id {
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
	if candidate.id != m.presented[threadID] {
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
