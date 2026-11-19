package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// Page lifecycle. A tab appears in the thread's state the moment a page is
// asked for and leaves it the moment it is closed; the engine work behind
// either runs after that, and every cap and the workspace's profile stay
// held until it ends.
//
//   - Create: the page is registered as pending (Creating in the companion
//     state) before the engine round trip, admitted against the thread's
//     cap there. It becomes live when the engine page exists. A creation the
//     user asked for that fails stays as a tab with its Error until closed;
//     any other failed creation leaves the tab set.
//   - Close: the page leaves scope.pages for scope.closing, its operations
//     are cancelled and the state is pushed, then runClose waits for its lock,
//     closes the driver, and releases its scope. A failure of that teardown
//     is a "teardown" event (reportTeardown); a caller that waits gets it too.
//   - Dispose: a scope released with its last page is disposed off the
//     caller (disposeReleased) while disposing holds the workspace, so a new
//     page there waits for it rather than opening a second profile.

// pendingPage is a page createPage is creating, or a failed creation a user
// tab keeps. Every field but p, done and cleanupErr is guarded by m.mu.
type pendingPage struct {
	p      *managedPage
	cancel context.CancelFunc
	// err is the failed creation, kept for a tab the user opened.
	err string
	// closed marks a creation closed while it ran: its tab is gone, and the
	// entry stays, counted toward its thread's cap, until the engine page it
	// may still produce is closed.
	closed bool
	// done closes when createPage has finished with the entry.
	done chan struct{}
	// cleanupErr is the failed teardown of a creation closed while it ran,
	// written before done closes.
	cleanupErr error
}

// wait answers the creation's cleanup failure once createPage is done.
func (pending *pendingPage) wait(ctx context.Context) error {
	select {
	case <-pending.done:
		return pending.cleanupErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// teardownFailure marks the part of a failed creation that is the failed
// disposal of the profile the failure left empty, which a creation closed
// while it ran still reports.
type teardownFailure struct{ err error }

func (e *teardownFailure) Error() string { return e.err.Error() }
func (e *teardownFailure) Unwrap() error { return e.err }

// asTeardown marks err as a teardown failure; nil stays nil.
func asTeardown(err error) error {
	if err == nil {
		return nil
	}
	return &teardownFailure{err: err}
}

// teardownPart answers the teardown failure inside err, or nil.
func teardownPart(err error) error {
	var failure *teardownFailure
	if errors.As(err, &failure) {
		return failure.err
	}
	return nil
}

// pageClose is one page's close in flight. err is written before done
// closes.
type pageClose struct {
	p    *managedPage
	done chan struct{}
	err  error
	// ended are the downloads the page left running (pageLeftLocked).
	ended endedDownloads
}

func (c *pageClose) wait(ctx context.Context) error {
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// errClosedWhileCreating answers a creation whose tab was closed before the
// engine page existed.
var errClosedWhileCreating = errors.New("browser: the page was closed while it was being created")

// maxPageErrorBytes bounds a failed creation's message on its tab.
const maxPageErrorBytes = 1 << 10

// disposeTimeout bounds the disposal of a profile released by a close.
const disposeTimeout = 20 * time.Second

// goLife runs fn on a goroutine Close joins, and reports false without
// running it once Close has begun waiting.
func (m *Manager) goLife(fn func()) bool {
	m.lifeGate.Lock()
	if m.lifeClosed {
		m.lifeGate.Unlock()
		return false
	}
	m.lifeWork.Add(1)
	m.lifeGate.Unlock()
	go func() {
		defer m.lifeWork.Done()
		fn()
	}()
	return true
}

// tabClaim is what a new tab takes besides its slot: a closed tab it
// reopens (ReopenClosedTab), taken off the thread's closed-tab history in the
// hold that adds the tab.
type tabClaim struct {
	stack *closedTabs
	tab   closedTab
}

// errReopenForgotten refuses a reopened tab whose closed-tab history was
// forgotten, or whose entry another reopen took, after the reopen read it.
var errReopenForgotten = errors.New("browser: the closed tab was forgotten")

// admitTabLocked is a new tab's admission in the hold that adds it and
// again in the startMu hold that creates its page: the thread lifetime ctx
// is bound to still holds (BindThread), and a reopened tab's history is
// still the thread's, which a clear's forget, a jar switch and the thread's
// close replace. The caller holds m.mu.
func (m *Manager) admitTabLocked(ctx context.Context, owner string, claim *tabClaim) error {
	if err := m.lifeAdmitsLocked(ctx, owner); err != nil {
		return err
	}
	if claim != nil && m.closedTabs[owner] != claim.stack {
		return errReopenForgotten
	}
	return nil
}

// admissionRefused is a tab admitTabLocked refused where its page is
// created.
type admissionRefused struct{ err error }

func (e *admissionRefused) Error() string { return e.err.Error() }
func (e *admissionRefused) Unwrap() error { return e.err }

// createPage creates a page for access's thread. Its tab appears at once,
// creating, and the call returns once the engine page exists. A tab closed
// meanwhile answers errClosedWhileCreating. This is an agent's tab, opened
// behind the selection; the user's go through createPageAt.
func (m *Manager) createPage(ctx context.Context, access Access) (*managedPage, error) {
	return m.createPageAt(ctx, access, nil, -1, nil)
}

// createPageAt is createPage with the new tab placed at index in the
// thread's tab order from the start; a negative index appends it. A tab the
// user opens has the turn its selection was admitted in (selection.go): it
// is selected and its companion shown before the engine is asked, in the
// hold that adds the tab and commits the turn, and a failure stays on the
// tab. A turn whose pane detached meanwhile opens nothing and answers
// errSelectionPassed. An agent's tab has no turn. claim, when set, is taken in
// the hold that adds the tab (admitTabLocked).
func (m *Manager) createPageAt(ctx context.Context, access Access, turn *selectionTurn, index int, claim *tabClaim) (*managedPage, error) {
	user := turn != nil
	p := newManagedPage(access)
	p.info = PageInfo{ID: p.id, URL: "about:blank"}
	createCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := &pendingPage{p: p, cancel: cancel, done: make(chan struct{})}
	defer close(pending.done)
	owner := access.ThreadID

	admit := func() error {
		if err := m.admitTabLocked(ctx, owner, claim); err != nil {
			return &admissionRefused{err: err}
		}
		return nil
	}
	m.mu.Lock()
	if err := m.admitTabLocked(ctx, owner, claim); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if claim != nil {
		if stack := claim.stack; len(stack.tabs) == 0 || stack.tabs[len(stack.tabs)-1] != claim.tab {
			// Another reopen took it.
			m.mu.Unlock()
			return nil, errReopenForgotten
		}
	}
	if user && !m.commitSelectionLocked(turn) {
		m.mu.Unlock()
		return nil, errSelectionPassed
	}
	if err := m.admitThreadPageLocked(owner); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if claim != nil {
		claim.stack.tabs = claim.stack.tabs[:len(claim.stack.tabs)-1]
	}
	if m.pending == nil {
		m.pending = make(map[string]*pendingPage)
	}
	m.pending[p.id] = pending
	savedMoved := false
	if index >= 0 {
		savedMoved, _ = m.placeTabLocked(owner, p.id, index)
	}
	if user {
		info := m.sessionLocked(owner)
		info.ActivePageID, info.Visible = p.id, true
		info.UpdatedAt = time.Now()
		m.sessions[owner] = info
	}
	m.mu.Unlock()
	m.emitThreadState(owner)
	if savedMoved {
		if err := m.persistThreads([]string{owner}); err != nil {
			// The suspended tabs keep their new places in memory; only a
			// restart before the next save puts them back.
			log.Printf("browser: place a new tab of thread %s: save the thread's pages: %v", owner, err)
		}
	}
	if user {
		// The page shown before is hidden while the new tab is created.
		m.syncPanePresentation(owner)
	}

	// One startMu hold spans the engine start and the page's registration, so
	// the idle close cannot stop the engine in between.
	var scope *jarScope
	var err error
	var closed bool
	var refused *admissionRefused
	for {
		m.startMu.Lock()
		scope, err = m.openPageLocked(createCtx, access, p, admit)
		refused = nil
		errors.As(err, &refused)
		// One m.mu hold settles the creation against its close flag and its
		// thread's jar as they are now, so neither a close nor a jar switch can
		// land between reading them and acting on it.
		m.mu.Lock()
		closed = pending.closed
		if err == nil && !closed && !m.jarCurrentLocked(p) {
			// The thread switched jars while the engine created the page in the
			// old one: it is created again, under the same tab, in the new one.
			stale := p
			p = m.freshPendingPageLocked(pending)
			m.mu.Unlock()
			m.startMu.Unlock()
			m.abandonStale(scope, stale, p)
			continue
		}
		switch {
		case err == nil && !closed:
			delete(m.pending, p.id)
			delete(scope.creating, p)
			m.listPageLocked(scope, p)
		case err != nil && (closed || !user || refused != nil):
			delete(m.pending, p.id)
		case err != nil:
			pending.err = truncateUTF8(err.Error(), maxPageErrorBytes)
		}
		m.mu.Unlock()
		m.startMu.Unlock()
		break
	}

	switch {
	case err == nil && !closed:
		// The tab leaves its creating state.
		m.pageAdded(p)
		return p, nil
	case err == nil:
		// Closed while the engine created it: the engine page is closed
		// before the entry stops holding the thread's slot.
		pending.cleanupErr = m.abandonPage(scope, p)
		m.dropPending(pending)
		if pending.cleanupErr != nil {
			m.reportTeardown(owner, p.id, pending.cleanupErr)
		}
		return nil, errClosedWhileCreating
	}
	if closed || user {
		// The profile the creation left behind failing to dispose is
		// reported whether or not its tab is still there: a close can take
		// the tab at any point from here, and then answers with it. The
		// creation's own failure (its cancellation, for a closed tab) is not.
		pending.cleanupErr = teardownPart(err)
		if pending.cleanupErr != nil {
			m.reportTeardown(owner, p.id, pending.cleanupErr)
		}
	}
	m.repairActivePage(owner)
	m.emitThreadState(owner)
	m.syncPanePresentation(owner)
	switch {
	case closed:
		return nil, errClosedWhileCreating
	case refused != nil:
		return nil, refused.err
	case user:
		return nil, &shownOnTab{err: err}
	}
	return nil, err
}

// freshPendingPageLocked replaces a pending tab's page, whose engine page was
// created under a jar its thread has left, with a new page under the same id
// and tab position, and answers it. The caller holds m.mu and abandons the
// old page (abandonStale).
func (m *Manager) freshPendingPageLocked(pending *pendingPage) *managedPage {
	old := pending.p
	p := newManagedPage(old.access)
	p.id, p.createdAt, p.info = old.id, old.createdAt, old.cachedInfo()
	p.tabOrder.Store(old.tabOrder.Load())
	p.lastUse.Store(old.lastUse.Load())
	pending.p = p
	return p
}

// abandonStale closes a page created in a jar its thread has left, before it
// was registered; a failure to release that jar's profile reports as the
// teardown of the tab's page.
func (m *Manager) abandonStale(scope *jarScope, stale, tab *managedPage) {
	if err := m.abandonPage(scope, stale); err != nil {
		m.reportTeardown(tab.owner, tab.id, err)
	}
}

// shownOnTab is a failed creation the user's tab shows as its Error.
type shownOnTab struct{ err error }

func (e *shownOnTab) Error() string { return e.err.Error() }
func (e *shownOnTab) Unwrap() error { return e.err }

// dropPending forgets a creation entry.
func (m *Manager) dropPending(pending *pendingPage) {
	m.mu.Lock()
	if m.pending[pending.p.id] == pending {
		delete(m.pending, pending.p.id)
	}
	m.mu.Unlock()
}

// admitThreadPageLocked refuses a page past the thread's cap. Every tab
// counts, and so does a page still closing or being created after its tab
// was closed. The caller holds m.mu.
func (m *Manager) admitThreadPageLocked(threadID string) error {
	if m.threadPageCountLocked(threadID) < maxPagesPerThread {
		return nil
	}
	closing := 0
	for _, scope := range m.scopes {
		for _, c := range scope.closing {
			if c.p.owner == threadID && m.suspended[c.p.id] == nil {
				closing++
			}
		}
	}
	for _, pending := range m.pending {
		if pending.closed && pending.p.owner == threadID {
			closing++
		}
	}
	return fmt.Errorf("browser: page limit reached for thread (%d)%s", maxPagesPerThread, closingHint(closing))
}

// closingHint says why a cap is reached while closed pages still hold it.
func closingHint(closing int) string {
	if closing > 0 {
		return "; a closed page is still shutting down, try again in a moment"
	}
	return "; close a page first"
}

// ClosePage closes one of the caller's pages and waits for the engine to
// finish with it, answering a failure of that teardown. A suspended page has
// no engine page to close; closing it forgets its record.
func (m *Manager) ClosePage(ctx context.Context, access Access, pageID string) error {
	return m.closePage(ctx, access, pageID, true, false)
}

// closePage closes a live, closing, pending or suspended page. A page whose
// close is already running answers that close, so repeated closes agree.
// byUser is the user's close, which the close that takes the tab out of the
// tab set records for reopening (recordUserCloseLocked). The tab is found in
// whichever of those states it is in, and taken out of it, in one m.mu hold,
// so a creation or restore moving it between states meanwhile cannot hide it.
func (m *Manager) closePage(ctx context.Context, access Access, pageID string, wait, byUser bool) error {
	pageID = strings.TrimSpace(pageID)
	m.mu.Lock()
	if m.forgetRecordLocked(access.ThreadID, pageID, byUser) {
		m.mu.Unlock()
		return m.recordForgotten(access.ThreadID)
	}
	c, started, err := m.beginCloseLiveLocked(access, pageID, byUser)
	if err == nil {
		m.mu.Unlock()
		if started != nil {
			m.startClose(started, c)
		}
		if !wait {
			return nil
		}
		return c.wait(ctx)
	}
	pending, ok := m.closePendingLocked(access, pageID)
	m.mu.Unlock()
	if !ok {
		return errPageNotFound
	}
	m.pendingClosed(access.ThreadID)
	if !wait {
		return nil
	}
	return pending.wait(ctx)
}

// closeLivePage closes a live page and waits for its teardown. It answers
// errPageNotFound when the page is not live or closing.
func (m *Manager) closeLivePage(ctx context.Context, access Access, pageID string) error {
	c, err := m.beginCloseLive(access, pageID, false)
	if err != nil {
		return err
	}
	return c.wait(ctx)
}

// beginCloseLive starts the close of the caller's live page, or answers the
// close already running for it (beginCloseLiveLocked).
func (m *Manager) beginCloseLive(access Access, pageID string, byUser bool) (*pageClose, error) {
	m.mu.Lock()
	c, started, err := m.beginCloseLiveLocked(access, pageID, byUser)
	m.mu.Unlock()
	if started != nil {
		m.startClose(started, c)
	}
	return c, err
}

// beginCloseLiveLocked moves the caller's live page to closing, answering
// its close and its scope, for the caller to startClose after releasing
// m.mu, or answers the close already running for it with a nil scope.
// Finding the page and moving it is one m.mu hold, so concurrent closes
// agree on one teardown, and only the one that moved it records a user's
// close. The caller holds m.mu.
func (m *Manager) beginCloseLiveLocked(access Access, pageID string, byUser bool) (*pageClose, *jarScope, error) {
	if m.suspended[pageID] != nil {
		// A jar switch's record: the tab is the record, and its page closing
		// under the same id is no longer a tab (SetJar).
		return nil, nil, errPageNotFound
	}
	for _, scope := range m.scopes {
		if c := scope.closing[pageID]; c != nil {
			if c.p.owner != access.ThreadID {
				return nil, nil, errPageNotFound
			}
			return c, nil, nil
		}
		if p := scope.pages[pageID]; p != nil {
			if p.owner != access.ThreadID {
				return nil, nil, errPageNotFound
			}
			if byUser {
				m.recordUserCloseLocked(p.owner, pageID)
			}
			return m.markClosingLocked(p, scope), scope, nil
		}
	}
	return nil, nil, errPageNotFound
}

// markClosingLocked moves a live page to its scope's closing pages. The
// caller holds m.mu and calls startClose after releasing it.
func (m *Manager) markClosingLocked(p *managedPage, scope *jarScope) *pageClose {
	c := &pageClose{p: p, done: make(chan struct{}), ended: m.pageLeftLocked(scope, p, "page closed")}
	if scope.closing == nil {
		scope.closing = make(map[string]*pageClose)
	}
	scope.closing[p.id] = c
	m.retirePrivateJarLocked(scope)
	return c
}

// startClose publishes a page's move to closing and runs its teardown.
func (m *Manager) startClose(scope *jarScope, c *pageClose) {
	p := c.p
	// An operation holding the page's lock gives it up; one waiting for it
	// finds the page gone (pageLive) or its operations ended.
	p.cancelOps()
	m.repairActivePage(p.owner)
	m.emitThreadState(p.owner)
	m.publishDownloads(c.ended.notes...)
	// Hides the page (presentPanes hides closing pages) before its engine
	// teardown, which can take a while.
	m.syncPanePresentation(p.owner)
	if !m.goLife(func() { m.runClose(scope, c) }) {
		m.runClose(scope, c)
	}
}

// runClose is a close's engine half. It waits for the page's lock, closes
// the driver, and releases the scope, disposing it when this was its last
// page.
func (m *Manager) runClose(scope *jarScope, c *pageClose) {
	p := c.p
	p.mu.Lock()
	m.stopEndedDownloads(c.ended)
	p.driver.Close()
	m.endFindLocked(p)
	p.mu.Unlock()
	m.mu.Lock()
	delete(scope.closing, p.id)
	release := m.releaseScopeLocked(scope)
	m.mu.Unlock()
	if release {
		ctx, cancel := context.WithTimeout(context.Background(), disposeTimeout)
		c.err = m.disposeScope(ctx, scope)
		cancel()
	}
	close(c.done)
	if c.err != nil {
		m.reportTeardown(p.owner, p.id, c.err)
	}
}

// closePending closes a tab that is still being created, or a failed
// creation's tab, and answers its entry, whose wait ends when the creation
// has finished with it; false when pageID is no such tab of the caller.
func (m *Manager) closePending(access Access, pageID string) (*pendingPage, bool) {
	m.mu.Lock()
	pending, ok := m.closePendingLocked(access, pageID)
	m.mu.Unlock()
	if ok {
		m.pendingClosed(access.ThreadID)
	}
	return pending, ok
}

// closePendingLocked is closePending's lock hold; the caller holds m.mu and
// calls pendingClosed after releasing it when it answers true.
func (m *Manager) closePendingLocked(access Access, pageID string) (*pendingPage, bool) {
	pending := m.pending[pageID]
	if pending == nil || pending.p.owner != access.ThreadID {
		return nil, false
	}
	if pending.err != "" {
		delete(m.pending, pageID)
	} else if !pending.closed {
		pending.closed = true
		pending.cancel()
	}
	return pending, true
}

// pendingClosed publishes the close of a pending tab of the thread.
func (m *Manager) pendingClosed(threadID string) {
	m.repairActivePage(threadID)
	m.emitThreadState(threadID)
	m.syncPanePresentation(threadID)
}

// closeThreadPending closes every tab of the thread that is still being
// created and forgets its failed creations, for CloseThread: a creation it
// closes registers no page.
func (m *Manager) closeThreadPending(threadID string) {
	changed := false
	m.mu.Lock()
	for id, pending := range m.pending {
		switch {
		case pending.p.owner != threadID:
		case pending.err != "":
			delete(m.pending, id)
			changed = true
		case !pending.closed:
			pending.closed = true
			pending.cancel()
			changed = true
		}
	}
	m.mu.Unlock()
	if changed {
		m.emitThreadState(threadID)
	}
}

// abandonPage closes a page openPageLocked created that will not be
// registered, ends its reservation, and disposes the scope if that left it
// empty.
func (m *Manager) abandonPage(scope *jarScope, p *managedPage) error {
	p.driver.Close()
	m.mu.Lock()
	// Never listed, so a popup its engine page opened is not adopted
	// (awaitOpener).
	m.unlistedLocked().put(p.driver.Handle(), struct{}{})
	m.listingChangedLocked()
	delete(scope.creating, p)
	release := m.releaseScopeLocked(scope)
	m.mu.Unlock()
	if release {
		return m.disposeScope(context.Background(), scope)
	}
	return nil
}

// releaseScopeLocked unregisters scope when it has no page, none closing and
// none being created, and reports whether the caller must dispose it; the jar
// is then disposing until disposeScope ends. Deciding and unregistering under
// one m.mu hold means a concurrent createPage either reserved the scope first
// or waits for the disposal, and a scope that is no longer registered is
// never disposed again. Every path a page leaves its scope by calls it, so it
// also forgets the authority of a workspace left with no page, and retires
// a private jar left with no tab. The caller holds m.mu.
func (m *Manager) releaseScopeLocked(scope *jarScope) bool {
	m.pruneAuthoritiesLocked()
	m.retirePrivateJarLocked(scope)
	if len(scope.pages) > 0 || len(scope.closing) > 0 || len(scope.creating) > 0 || m.scopes[scope.jar.key] != scope {
		return false
	}
	delete(m.scopes, scope.jar.key)
	if m.disposing == nil {
		m.disposing = make(map[string]chan struct{})
	}
	m.disposing[scope.jar.key] = make(chan struct{})
	return true
}

// retirePrivateJarLocked ends the thread's use of a private jar once no tab
// of it is live or being created: the thread's next private page opens a new
// jar, while pages still closing, and popups still being adopted, finish in
// the old one, under its own key. A popup is listed only while its opener is
// a tab of the jar (adoptPopup), so one being adopted keeps no tab coming.
// The caller holds m.mu.
func (m *Manager) retirePrivateJarLocked(scope *jarScope) {
	if scope.jar.private == "" || len(scope.pages) > 0 {
		return
	}
	for p := range scope.creating {
		if !m.adoptingLocked(p) {
			return
		}
	}
	if info, ok := m.sessions[scope.jar.thread]; ok && info.privateJar == scope.jar.private {
		info.privateJar = ""
		m.sessions[scope.jar.thread] = info
	}
}

// adoptingLocked reports whether p is the page a popup's adoption reserves.
// The caller holds m.mu.
func (m *Manager) adoptingLocked(p *managedPage) bool {
	for _, adopted := range m.adopting {
		if adopted == p {
			return true
		}
	}
	return false
}

// disposeScope disposes a scope releaseScopeLocked unregistered, frees its
// jar for a new profile, and starts the idle close once no scope
// remains.
func (m *Manager) disposeScope(ctx context.Context, scope *jarScope) error {
	err := scope.profile.Dispose(ctx)
	m.mu.Lock()
	if done := m.disposing[scope.jar.key]; done != nil {
		delete(m.disposing, scope.jar.key)
		close(done)
	}
	noScopes := len(m.scopes) == 0 && len(m.disposing) == 0
	m.mu.Unlock()
	if noScopes {
		m.scheduleIdleClose()
	}
	return err
}

// disposeReleased stops the downloads a page closed by its engine left
// running and disposes the scope its close released (nil for none), off the
// caller, reporting a failure as the teardown of that page.
func (m *Manager) disposeReleased(ended endedDownloads, scope *jarScope, threadID, pageID string) {
	run := func() {
		m.stopEndedDownloads(ended)
		if scope == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), disposeTimeout)
		defer cancel()
		if err := m.disposeScope(ctx, scope); err != nil {
			m.reportTeardown(threadID, pageID, err)
		}
	}
	if !m.goLife(run) {
		run()
	}
}

// reportTeardown publishes the failure of a teardown that ran after its page
// left the tab set, for the pane's banner or, with no pane, a toast.
func (m *Manager) reportTeardown(threadID, pageID string, err error) {
	log.Printf("browser: close page %s of thread %s: %v", pageID, threadID, err)
	m.emit(CompanionEvent{Kind: "teardown", ThreadID: threadID, PageID: pageID, Error: err.Error()})
}
