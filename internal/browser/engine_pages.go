package browser

import (
	"context"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/browser/ids"
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
func (m *Manager) pageClosed(handle ids.PageHandle) {
	m.pageEnds.report(handle, func() bool {
		m.admissions.Observe(handle, popupadmit.Closed())
		return true
	}, func() { m.removeClosedPage(handle) })
}

// pageEndReceived is engineEvents.PageEndReceived: the close or crash is
// recorded in the popup's admission now; its PageClosed or PageCrashed
// follows and acts.
func (m *Manager) pageEndReceived(handle ids.PageHandle, end popupadmit.Observation) {
	m.admissions.Observe(handle, end)
}

// listingDriver is a page driver whose engine schedules a popup's work by
// whether the Manager listed it (the hosted engine's report lanes): the
// listing tells it, after its commit.
type listingDriver interface {
	Listed()
}

// pageCrashedReport is engineEvents.PageCrashed. A popup not listed yet
// keeps the crash for its listing, which lists it crashed unless it also
// closed; any other page is handled as a crashed tab (pageCrashed).
func (m *Manager) pageCrashedReport(handle ids.PageHandle, reason string) {
	m.pageEnds.report(handle, func() bool {
		return m.admissions.Observe(handle, popupadmit.Crashed(reason)) == popupadmit.Unheld
	}, func() { m.pageCrashed(handle, reason) })
}

// pageEndQueue runs what the Manager does about each page's reported
// close and crash, one page's in the order they were reported, on a
// goroutine that exists while the page has work queued. Pages' work runs
// apart, so one page's teardown never waits for another's.
type pageEndQueue struct {
	mu   sync.Mutex
	jobs map[ids.PageHandle][]func()
}

// report records a report of page handle (record, which answers whether
// it leaves work to do) and queues that work behind the page's earlier
// work, in one hold, so the work runs in the order the records were made.
func (q *pageEndQueue) report(handle ids.PageHandle, record func() bool, work func()) {
	q.mu.Lock()
	if !record() {
		q.mu.Unlock()
		return
	}
	if q.jobs == nil {
		q.jobs = make(map[ids.PageHandle][]func())
	}
	queued, running := q.jobs[handle]
	q.jobs[handle] = append(queued, work)
	q.mu.Unlock()
	if !running {
		go q.run(handle)
	}
}

// run does the queued work of handle until none is left.
func (q *pageEndQueue) run(handle ids.PageHandle) {
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
	if !m.admissions.Announce(popup.Handle, popup.Opener) {
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
	p := newManagedPage(opener.owner, opener.access)
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
		// Its admission decides even so: a popup whose process ended
		// before its attach failed is the crashed tab its crash makes it.
		if m.listUnattachedPopup(scope, p, popup) {
			reserved = false
		}
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
	// The listing waits out a jar switch of the opener's lifetime
	// (lockStructure), whose commit closes the opener. Without a cancel: a
	// switch ends with its one write.
	_ = m.lockStructure(context.Background(), opener.owner)
	// The opener may have closed meanwhile, with its thread's other pages
	// (popupListableLocked). The thread's close is one such close: it
	// closes every live page of the lifetime it detached until none is
	// left, a popup listed after the detach included. The caps were taken
	// at admission by the reservation the listing ends. The commit, in the
	// same hold, reads what ended the popup before it: a close refuses it,
	// a crash alone lists it crashed, never live first.
	if !m.popupListableLocked(scope, popup) {
		m.mu.Unlock()
		m.parkMu.Unlock()
		driver.Close()
		return
	}
	verdict := m.admissions.Commit(popup.Handle)
	switch verdict.Kind() {
	case popupadmit.List:
	case popupadmit.ListCrashed:
		listedCrashed, released := m.listCrashedPopupLocked(scope, p, popup.Handle, verdict.CrashReason())
		reserved = !listedCrashed
		m.mu.Unlock()
		m.parkMu.Unlock()
		driver.Close()
		if listedCrashed {
			m.crashedPopupListed(p, released)
		}
		return
	case popupadmit.Refuse:
		m.mu.Unlock()
		m.parkMu.Unlock()
		driver.Close()
		return
	default:
		panic("browser: a popup commit of unknown kind")
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
	if listing, ok := driver.(listingDriver); ok {
		listing.Listed()
	}
	if m.popupListing != nil {
		m.popupListing()
	}
	// Its downloads, before anything the engine held for the listing can
	// close it.
	m.resolveParkedLocked(popup.Handle, p, popup.Opener)
	m.parkMu.Unlock()
	// A new tab.
	m.pageAdded(p)
	if m.pageAdopted != nil {
		m.pageAdopted()
	}
}

// popupListableLocked answers whether a popup may still be listed in
// scope: its jar is still open and its opener still a tab of it. A jar
// switch moves every live page of the thread to closing in the hold that
// advances the generation, so a live opener is always in its thread's
// current jar and the popup is too. The caller holds m.mu.
func (m *Manager) popupListableLocked(scope *jarScope, popup enginePopup) bool {
	return m.scopes[scope.jar.key] == scope && pageWithHandle(scope, popup.Opener) != nil
}

// listUnattachedPopup commits the admission of a popup whose attach
// failed, and lists it as a crashed tab when its process was seen to end
// first (popupadmit.Registry.CommitUnattached). It answers whether it did,
// which ends the popup's reservation.
func (m *Manager) listUnattachedPopup(scope *jarScope, p *managedPage, popup enginePopup) bool {
	m.parkMu.Lock()
	// A crashed tab is a change to the lifetime's tabs, made after any jar
	// switch of it (lockStructure).
	_ = m.lockStructure(context.Background(), p.owner)
	if !m.popupListableLocked(scope, popup) {
		m.mu.Unlock()
		m.parkMu.Unlock()
		return false
	}
	verdict := m.admissions.CommitUnattached(popup.Handle)
	listed, released := false, (*jarScope)(nil)
	if verdict.Kind() == popupadmit.ListCrashed {
		listed, released = m.listCrashedPopupLocked(scope, p, popup.Handle, verdict.CrashReason())
	}
	m.mu.Unlock()
	m.parkMu.Unlock()
	if listed {
		m.crashedPopupListed(p, released)
	}
	return listed
}

// listCrashedPopupLocked lists popup p, whose process ended before its
// listing, as the crashed tab it is: a suspended record carrying the
// failure (crash.go), in the hold that ends its reservation, with no live
// listing before it. What it downloaded goes to its opener's tab
// (finishAdoption). As a crash of a listed page does, it lists nothing
// while the Manager closes or forgets every page, or for a thread lifetime
// its close detached, whose records that close already dropped. It answers
// whether it listed the popup, and the jar to dispose when the reservation
// was the last thing holding it. The caller holds m.parkMu and m.mu.
func (m *Manager) listCrashedPopupLocked(scope *jarScope, p *managedPage, handle ids.PageHandle, reason string) (bool, *jarScope) {
	if m.closed || m.forgettingLocked() || p.owner.detached.Load() {
		return false, nil
	}
	delete(scope.creating, p)
	m.adopting[handle] = nil
	m.commitHeldInfoLocked(p, handle)
	rec := recordOf(p, p.cachedInfo())
	rec.failure = crashFailure(reason)
	m.suspended[p.id] = rec
	if m.releaseScopeLocked(scope) {
		return true, scope
	}
	return true, nil
}

// crashedPopupListed publishes a popup listed crashed
// (listCrashedPopupLocked), as a crash of a listed page publishes its tab,
// and saves its thread's tabs.
func (m *Manager) crashedPopupListed(p *managedPage, released *jarScope) {
	log.Printf("browser: popup %s of thread %s crashed before its listing", p.id, p.owner.id)
	if released != nil {
		m.disposeReleased(endedDownloads{}, released, p.owner.id, p.id)
	}
	m.ensureActivePage(p.owner, p.id)
	m.emitThreadState(p.owner.id)
	m.syncPanePresentation(p.owner.id)
	if err := m.persistThreads([]string{p.owner.id}); err != nil {
		// The tab is in memory; only a restart before the next save loses it.
		log.Printf("browser: popup %s crashed: save the thread's pages: %v", p.id, err)
	}
	if m.pageAdopted != nil {
		m.pageAdopted()
	}
}

// discardPopup has the engine close a popup the Manager will not list,
// within popupDiscardTimeout. Its reservation ends whatever the engine
// answers (endPopupReservation); a page the engine failed to close stays
// the engine's, closed at its profile's disposal.
func (m *Manager) discardPopup(handle ids.PageHandle) {
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
func (m *Manager) endPopupReservation(scope *jarScope, p *managedPage, handle ids.PageHandle) {
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
		m.reportTeardown(p.owner.id, p.id, err)
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
func pageWithHandle(scope *jarScope, handle ids.PageHandle) *managedPage {
	for _, p := range scope.pages {
		if p.driver.Handle() == handle {
			return p
		}
	}
	return nil
}

// removeClosedPage is the teardown of a page whose engine reported its
// close (pageClosed).
func (m *Manager) removeClosedPage(handle ids.PageHandle) {
	// Before m.mu, so a popup's tab is taken only once the downloads its
	// listing owes it are in it (adoptPopup).
	m.parkMu.Lock()
	// The page's tab leaves in a hold that waits out a jar switch of its
	// lifetime (lockStructure), whose commit may close it first. Without a
	// cancel: a switch ends with its one write.
	m.mu.Lock()
	p := m.pageByHandleLocked(handle)
	m.mu.Unlock()
	if p != nil {
		_ = m.lockStructure(context.Background(), p.owner)
	} else {
		m.mu.Lock()
	}
	var owner *threadBrowser
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
		m.disposeReleased(ended, released, owner.id, closed.id)
	}
	if owner != nil {
		m.repairActivePage(owner)
		m.emitThreadState(owner.id)
		m.syncPanePresentation(owner.id)
	}
}
