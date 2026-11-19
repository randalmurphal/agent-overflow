package browser

import (
	"context"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/browser/popupadmit"
)

// Pages an engine reports: a popup it opened by itself (popupOpened), and
// the close or crash of any page (pageClosed, pageCrashedReport). Each
// enters the popup's admission (popupadmit) in the engine's call, before
// anything the Manager does about it starts, so the adoption's one commit
// at the listing reads every close and crash the engine reported before
// it. Each returns at once and acts on a goroutine of its own, so an
// engine may call it on its UI thread or its event reader; what a page's
// close and crash do runs in the order they were reported (pageEndQueue),
// so a listed tab that closed and then crashed is gone, not crashed.

// popupOpened is engineEvents.PopupOpened: it registers the popup's
// admission and adopts it on a goroutine of its own.
func (m *Manager) popupOpened(popup enginePopup) {
	if !m.announcePopup(popup) {
		return
	}
	go m.runAdoption(popup)
}

// pageClosed is engineEvents.PageClosed. A popup not listed yet keeps the
// close for its listing, which it vetoes; the teardown runs either way
// (removeClosedPage), which for a popup never listed lists what it
// downloaded in its opener's tab at once.
func (m *Manager) pageClosed(handle string) {
	m.pageEnds.report(handle, func() bool {
		m.admissions.Observe(popupadmit.Handle(handle), popupadmit.Closed())
		return true
	}, func() { m.removeClosedPage(handle) })
}

// pageCrashedReport is engineEvents.PageCrashed. A popup not listed yet
// keeps the crash for its listing, which lists it crashed unless it also
// closed; any other page is handled as a crashed tab (pageCrashed).
func (m *Manager) pageCrashedReport(handle, reason string) {
	m.pageEnds.report(handle, func() bool {
		return m.admissions.Observe(popupadmit.Handle(handle), popupadmit.Crashed(reason)) == popupadmit.Unheld
	}, func() { m.pageCrashed(handle, reason) })
}

// pageEndQueue runs what the Manager does about each page's reported
// close and crash, one page's in the order they were reported, on a
// goroutine that exists while the page has work queued. Pages' work runs
// apart, so one page's teardown never waits for another's.
type pageEndQueue struct {
	mu   sync.Mutex
	jobs map[string][]func()
}

// report records a report of page handle (record, which answers whether
// it leaves work to do) and queues that work behind the page's earlier
// work, in one hold, so the work runs in the order the records were made.
func (q *pageEndQueue) report(handle string, record func() bool, work func()) {
	q.mu.Lock()
	if !record() {
		q.mu.Unlock()
		return
	}
	if q.jobs == nil {
		q.jobs = make(map[string][]func())
	}
	queued, running := q.jobs[handle]
	q.jobs[handle] = append(queued, work)
	q.mu.Unlock()
	if !running {
		go q.run(handle)
	}
}

// run does the queued work of handle until none is left.
func (q *pageEndQueue) run(handle string) {
	for {
		q.mu.Lock()
		jobs := q.jobs[handle]
		if len(jobs) == 0 {
			delete(q.jobs, handle)
			q.mu.Unlock()
			return
		}
		work := jobs[0]
		jobs[0] = nil
		q.jobs[handle] = jobs[1:]
		q.mu.Unlock()
		work()
	}
}

// adoptPopup announces popup and adopts it in the caller's goroutine.
func (m *Manager) adoptPopup(popup enginePopup) {
	if m.announcePopup(popup) {
		m.runAdoption(popup)
	}
}

// announcePopup registers popup's admission and answers false for a popup
// reported before.
func (m *Manager) announcePopup(popup enginePopup) bool {
	if !m.admissions.Announce(popupadmit.Handle(popup.Handle), popupadmit.Handle(popup.Opener)) {
		log.Printf("browser: popup %s reported twice", popup.Handle)
		return false
	}
	return true
}

// runAdoption takes ownership of a page the engine opened by itself. The
// opener decides the thread, the workspace authority and the jar; this
// manager's own limits decide whether it survives, and the popup's
// admission whether it closed meanwhile.
//
// A popup opened by a popup still being adopted waits for that adoption
// (awaitOpener), and what the popup downloads before its fate is listed at
// its end (finishAdoption). From the hold that admits it to its listing,
// the popup's page reserves its jar's profile, as a page being created
// does (scope.creating), and counts against every cap, so a close of its
// opener cannot dispose the profile it is attached in and a burst of
// popups cannot pass the caps before any of them is listed.
func (m *Manager) runAdoption(popup enginePopup) {
	m.startAdoption(popup.Handle)
	var listed *managedPage
	defer func() { m.finishAdoption(popup.Handle, popup.Opener, listed) }()
	if !m.awaitOpener(popup.Opener) {
		m.discardPopup(popup.Handle)
		return
	}
	m.mu.Lock()
	var scope *jarScope
	var opener *managedPage
	for _, candidate := range m.scopes {
		if candidate.profile.Handle() != popup.Profile {
			continue
		}
		scope, opener = candidate, pageWithHandle(candidate, popup.Opener)
		break
	}
	if opener == nil || m.popupRefusedLocked(scope, opener) {
		m.mu.Unlock()
		m.discardPopup(popup.Handle)
		return
	}
	p := newManagedPage(opener.access)
	p.jarGen, p.authority = opener.jarGen, opener.authority
	p.info = PageInfo{ID: p.id, URL: truncateUTF8(popup.URL, maxBrowserURLBytes), Title: truncateUTF8(popup.Title, maxBrowserTitleBytes)}
	scope.creating[p] = struct{}{}
	m.adopting[popup.Handle] = p
	m.mu.Unlock()
	reserved := true
	defer func() {
		if reserved {
			m.endPopupReservation(scope, p, popup.Handle)
		}
	}()

	p.touch()
	driver, err := scope.profile.AttachPage(context.Background(), popup.Handle, m.pageHooks(p))
	if err != nil {
		log.Printf("browser: adopt popup %s: %v", popup.Handle, err)
		m.discardPopup(popup.Handle)
		return
	}
	// Under m.mu: the jar's policy resolves a request to the reserved page
	// (jarAllows) while it loads, before it is listed.
	m.mu.Lock()
	p.attach(driver)
	held, hasHeld := m.takeHeldInfoLocked(popup.Handle)
	m.mu.Unlock()
	m.applyHeldInfo(p, held, hasHeld)
	if err := m.applyViewport(p); err != nil {
		log.Printf("browser: size popup %s: %v", popup.Handle, err)
		driver.Close()
		return
	}
	m.pageAttached(p)
	// The popup may have navigated before its listing, which reads no
	// history of its own: its Back and Forward go in the listing's push.
	m.readAdoptedHistory(p)
	// The tab and the downloads the popup started are listed in one
	// m.parkMu hold, which a page's close takes too (removeClosedPage), so
	// a close never takes the tab before its downloads are in it.
	m.parkMu.Lock()
	m.mu.Lock()
	// The opener may have closed meanwhile, with its thread's other pages. A
	// jar switch is one such close: it moves every live page of the thread
	// to closing in the hold that advances the generation, so a live opener
	// is always in its thread's current jar and the popup is too. The caps
	// were taken at admission by the reservation the listing ends. The
	// commit, in the same hold, reads what ended the popup before it: a
	// close refuses it, a crash alone lists it crashed.
	if m.scopes[scope.jar.key] != scope || pageWithHandle(scope, popup.Opener) == nil {
		m.mu.Unlock()
		m.parkMu.Unlock()
		driver.Close()
		return
	}
	verdict := m.admissions.Commit(popupadmit.Handle(popup.Handle))
	if verdict.Kind() == popupadmit.Refuse {
		m.mu.Unlock()
		m.parkMu.Unlock()
		driver.Close()
		return
	}
	delete(scope.creating, p)
	m.adopting[popup.Handle] = nil
	reserved = false
	m.listPageLocked(scope, p)
	listed = p
	// A report made while the popup was adopted joins the listing, whose
	// push carries it.
	m.commitHeldInfoLocked(p, popup.Handle)
	m.mu.Unlock()
	if m.popupListing != nil {
		m.popupListing()
	}
	// Its downloads, before anything the engine held for the listing can
	// close it.
	m.resolveParkedLocked(popup.Handle, p, popup.Opener)
	m.parkMu.Unlock()
	// A new tab.
	m.pageAdded(p)
	if verdict.Kind() == popupadmit.ListCrashed {
		// Its process ended before its listing: the tab is a crashed one,
		// as any listed page's would be.
		m.pageCrashed(popup.Handle, verdict.CrashReason())
	}
	if m.pageAdopted != nil {
		m.pageAdopted()
	}
}

// discardPopup has the engine close a popup the Manager will not list,
// within popupDiscardTimeout. Its reservation ends whatever the engine
// answers (endPopupReservation); a page the engine failed to close stays
// the engine's, closed at its profile's disposal.
func (m *Manager) discardPopup(handle string) {
	ctx, cancel := context.WithTimeout(context.Background(), popupDiscardTimeout)
	defer cancel()
	if err := m.engine.DiscardPage(ctx, handle); err != nil {
		log.Printf("browser: discard popup %s: %v", handle, err)
	}
}

// popupDiscardTimeout bounds an engine's discard of a popup.
const popupDiscardTimeout = 5 * time.Second

// endPopupReservation ends the reservation of a popup's page that will not
// be listed, its engine page already closed or discarded, and disposes the
// jar's profile if that was the last thing holding it.
func (m *Manager) endPopupReservation(scope *jarScope, p *managedPage, handle string) {
	m.mu.Lock()
	delete(scope.creating, p)
	if m.adopting[handle] == p {
		m.adopting[handle] = nil
	}
	release := m.releaseScopeLocked(scope)
	m.mu.Unlock()
	if !release {
		return
	}
	if err := m.disposeScope(context.Background(), scope); err != nil {
		m.reportTeardown(p.owner, p.id, err)
	}
}

// readAdoptedHistory reads an adopted popup's back and forward availability
// before its listing, under a stamp taken first, so a later report's own
// read stands. A failed read leaves both off until the next navigation's.
func (m *Manager) readAdoptedHistory(p *managedPage) {
	stamp := p.readStamp()
	ctx, cancel := operationContext(context.Background(), p.driver.Lifetime(), operationTimeout)
	defer cancel()
	back, forward, err := p.driver.HistoryState(ctx)
	if err != nil {
		log.Printf("browser: read the history of popup %s: %v", p.driver.Handle(), err)
		return
	}
	m.commitRead(p, pageUpdate{History: &pageHistory{CanGoBack: back, CanGoForward: forward}}, stamp)
}

// popupRefusedLocked reports whether a popup of p's thread, workspace and jar
// would pass a cap. The caller holds m.mu.
func (m *Manager) popupRefusedLocked(scope *jarScope, p *managedPage) bool {
	load, _ := m.workspaceLoadLocked(p.access.Workspace)
	return m.threadPageCountLocked(p.owner) >= maxPagesPerThread ||
		load >= maxPagesPerWorkspace || scopeLoadLocked(scope) >= maxPagesPerWorkspace ||
		countPagesLocked(m.scopes) >= maxPagesTotal
}

// pageWithHandle returns the scope's page whose driver has handle, or nil.
// The caller holds m.mu.
func pageWithHandle(scope *jarScope, handle string) *managedPage {
	for _, p := range scope.pages {
		if p.driver.Handle() == handle {
			return p
		}
	}
	return nil
}

// removeClosedPage is the teardown of a page whose engine reported its
// close (pageClosed).
func (m *Manager) removeClosedPage(handle string) {
	// Before m.mu, so a popup's tab is taken only once the downloads its
	// listing owes it are in it (adoptPopup).
	m.parkMu.Lock()
	m.mu.Lock()
	var owner string
	var closed *managedPage
	var released *jarScope
	var ended endedDownloads
	for _, scope := range m.scopes {
		if p := pageWithHandle(scope, handle); p != nil {
			owner, closed = p.owner, p
			ended = m.pageLeftLocked(scope, p, "page closed")
			if m.releaseScopeLocked(scope) {
				released = scope
			}
			break
		}
	}
	m.mu.Unlock()
	if closed == nil {
		// A popup never listed: what it downloaded goes to its opener's tab.
		m.resolveParkedLocked(handle, nil)
	}
	m.parkMu.Unlock()
	if closed != nil {
		m.publishDownloads(ended.notes...)
		m.endFind(closed)
		// Off the engine's callback, as a closed page's disposal always is,
		// and started before the state push: a page opened in the workspace
		// on that push waits for it.
		m.disposeReleased(ended, released, owner, closed.id)
	}
	if owner != "" {
		m.repairActivePage(owner)
		m.emitThreadState(owner)
		m.syncPanePresentation(owner)
	}
}
