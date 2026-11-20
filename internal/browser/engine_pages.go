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
		m.noteCreationEnd(handle, true, "")
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
		m.noteCreationEnd(handle, false, reason)
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
	if err := m.persistThreads([]ThreadID{p.owner.id}); err != nil {
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
