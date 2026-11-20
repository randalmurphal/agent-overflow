package browser

import (
	"context"
	"errors"
	"log"
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

// beginLife admits one piece of work Close joins and answers its end,
// which the caller calls once, or false once Close has begun waiting.
func (m *Manager) beginLife() (done func(), ok bool) {
	m.lifeGate.Lock()
	defer m.lifeGate.Unlock()
	if m.lifeClosed {
		return nil, false
	}
	m.lifeWork.Add(1)
	return m.lifeWork.Done, true
}

// goLife runs fn on a goroutine Close joins, and reports false without
// running it once Close has begun waiting.
func (m *Manager) goLife(fn func()) bool {
	done, ok := m.beginLife()
	if !ok {
		return false
	}
	go func() {
		defer done()
		fn()
	}()
	return true
}

// ClosePage closes one of the caller's pages and waits for the engine to
// finish with it, answering a failure of that teardown. A suspended page has
// no engine page to close; closing it forgets its record.
func (m *Manager) ClosePage(ctx context.Context, access Access, pageID PageID) error {
	if err := m.bound(access); err != nil {
		return err
	}
	return m.closePage(ctx, access, pageID, true, false)
}

// closePage closes a live, closing, pending or suspended page. A page whose
// close is already running answers that close, so repeated closes agree.
// byUser is the user's close, which the close that takes the tab out of the
// tab set records for reopening (recordUserCloseLocked). The tab is found in
// whichever of those states it is in, and taken out of it, in one m.mu hold,
// so a creation or restore moving it between states meanwhile cannot hide it.
// That hold waits for a jar switch of the thread writing its saved copy.
func (m *Manager) closePage(ctx context.Context, access Access, pageID PageID, wait, byUser bool) error {
	pageID = pageID.trim()
	if err := m.lockStructure(ctx, access.tb); err != nil {
		return err
	}
	finish := m.closeTabLocked(access.tb, pageID, byUser)
	m.mu.Unlock()
	return finish(ctx, wait)
}

// closeTabLocked is closePage's m.mu hold: it takes tb's tab pageID out of
// the tab set and answers the rest of the close, which the caller runs
// after releasing m.mu. The rest publishes the close and, with wait, waits
// for the teardown; it answers errPageNotFound when pageID was no tab of
// tb. The caller holds m.mu, taken by lockStructure.
func (m *Manager) closeTabLocked(tb *threadBrowser, pageID PageID, byUser bool) func(ctx context.Context, wait bool) error {
	if m.forgetRecordLocked(tb, pageID, byUser) {
		return func(context.Context, bool) error { return m.recordForgotten(tb) }
	}
	if c, started, err := m.beginCloseLiveLocked(tb, pageID, byUser); err == nil {
		return func(ctx context.Context, wait bool) error {
			if started != nil {
				m.startClose(started, c)
			}
			if !wait {
				return nil
			}
			return c.wait(ctx)
		}
	}
	pending, ok := m.closePendingLocked(tb, pageID)
	if !ok {
		return func(context.Context, bool) error { return errPageNotFound }
	}
	return func(ctx context.Context, wait bool) error {
		m.pendingClosed(tb)
		if !wait {
			return nil
		}
		return pending.wait(ctx)
	}
}

// beginCloseLiveLocked moves tb's live page to closing, answering its close
// and its scope, for the caller to startClose after releasing m.mu, or
// answers the close already running for it with a nil scope. Finding the
// page and moving it is one m.mu hold, so concurrent closes agree on one
// teardown, and only the one that moved it records a user's close. The
// caller holds m.mu.
func (m *Manager) beginCloseLiveLocked(tb *threadBrowser, pageID PageID, byUser bool) (*pageClose, *jarScope, error) {
	if m.suspended[pageID] != nil {
		// A jar switch's record: the tab is the record, and its page closing
		// under the same id is no longer a tab (SetJar).
		return nil, nil, errPageNotFound
	}
	for _, scope := range m.scopes {
		if c := scope.closing[pageID]; c != nil {
			if c.p.owner != tb {
				return nil, nil, errPageNotFound
			}
			return c, nil, nil
		}
		if p := scope.pages[pageID]; p != nil {
			if p.owner != tb {
				return nil, nil, errPageNotFound
			}
			if byUser {
				m.recordUserCloseLocked(tb, pageID)
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
		scope.closing = make(map[PageID]*pageClose)
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
	m.emitThreadState(p.owner.id)
	m.publishDownloads(c.ended.notes...)
	// Hides the page (presentPanes hides closing pages) before its engine
	// teardown, which can take a while.
	m.syncPanePresentation(p.owner.id)
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
		m.reportTeardown(p.owner.id, p.id, c.err)
	}
}

// closePendingLocked takes tb's tab pageID that is still being created,
// or a failed creation's tab, out of the tab set and answers its entry,
// whose wait ends when the creation has finished with it; false when pageID
// is no such tab of tb. The caller holds m.mu and calls pendingClosed after
// releasing it when it answers true.
func (m *Manager) closePendingLocked(tb *threadBrowser, pageID PageID) (*pendingPage, bool) {
	pending := m.pending[pageID]
	if pending == nil || pending.p.owner != tb {
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

// pendingClosed publishes the close of a pending tab of tb.
func (m *Manager) pendingClosed(tb *threadBrowser) {
	m.repairActivePage(tb)
	m.emitThreadState(tb.id)
	m.syncPanePresentation(tb.id)
}

// abandonPage closes a page openPageLocked created that will not be
// registered, ends its reservation, and disposes the scope if that left it
// empty. What the page downloaded while it loaded settles first, as a
// closed tab's does (downloadsLeftLocked), while its engine page still
// answers for what it received.
func (m *Manager) abandonPage(scope *jarScope, p *managedPage) error {
	m.mu.Lock()
	ended := m.downloadsLeftLocked(scope, p, "page closed")
	m.mu.Unlock()
	m.publishDownloads(ended.notes...)
	m.stopEndedDownloads(ended)
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
		m.disposing = make(map[jarKey]chan struct{})
	}
	m.disposing[scope.jar.key] = make(chan struct{})
	return true
}

// retirePrivateJarLocked ends the thread lifetime's use of a private jar
// once no tab of it is live or being created: its next private page opens a
// new jar, while pages still closing, and popups still being adopted, finish in
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
	if tb := scope.jar.owner; tb.session.privateJar == scope.jar.private {
		tb.session.privateJar = ""
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
func (m *Manager) disposeReleased(ended endedDownloads, scope *jarScope, threadID ThreadID, pageID PageID) {
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
func (m *Manager) reportTeardown(threadID ThreadID, pageID PageID, err error) {
	log.Printf("browser: close page %s of thread %s: %v", pageID, threadID, err)
	m.emit(CompanionEvent{Kind: "teardown", ThreadID: string(threadID), PageID: string(pageID), Error: err.Error()})
}
