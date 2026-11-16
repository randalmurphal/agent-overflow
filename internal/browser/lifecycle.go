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
// tab keeps. Every field but p and done is guarded by m.mu.
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
}

// pageClose is one page's close in flight. err is written before done
// closes.
type pageClose struct {
	p    *managedPage
	done chan struct{}
	err  error
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

// createPage creates a page for access's thread. Its tab appears at once,
// creating, and the call returns once the engine page exists. user marks a
// tab the user opened: it is selected and its companion shown before the
// engine is asked, and a failure stays on the tab. A tab closed meanwhile
// answers errClosedWhileCreating.
func (m *Manager) createPage(ctx context.Context, access Access, user bool) (*managedPage, error) {
	p := newManagedPage(access)
	p.info = PageInfo{ID: p.id, URL: "about:blank"}
	createCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := &pendingPage{p: p, cancel: cancel, done: make(chan struct{})}
	defer close(pending.done)
	owner := access.ThreadID

	m.mu.Lock()
	if err := m.admitThreadPageLocked(owner); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if m.pending == nil {
		m.pending = make(map[string]*pendingPage)
	}
	m.pending[p.id] = pending
	if user {
		info := m.sessionLocked(owner)
		info.ActivePageID, info.Visible = p.id, true
		info.UpdatedAt = time.Now()
		m.sessions[owner] = info
	}
	m.mu.Unlock()
	m.emitThreadState(owner)
	if user {
		// The page shown before is hidden while the new tab is created.
		m.syncPanePresentation(owner)
	}

	// One startMu hold spans the engine start and the page's registration, so
	// the idle close cannot stop the engine in between.
	m.startMu.Lock()
	scope, err := m.openPageLocked(createCtx, access, p)
	m.mu.Lock()
	closed := pending.closed
	if err == nil && !closed {
		delete(m.pending, p.id)
		scope.creating--
		scope.pages[p.id] = p
	}
	m.mu.Unlock()
	m.startMu.Unlock()

	switch {
	case err == nil && !closed:
		m.pageChanged(p)
		return p, nil
	case err == nil:
		// Closed while the engine created it: the engine page is closed
		// before the entry stops holding the thread's slot.
		abandonErr := m.abandonPage(scope, p)
		m.dropPending(pending)
		if abandonErr != nil {
			m.reportTeardown(owner, p.id, abandonErr)
		}
		return nil, errClosedWhileCreating
	}
	m.mu.Lock()
	if closed || !user {
		delete(m.pending, p.id)
	} else {
		pending.err = truncateUTF8(err.Error(), maxPageErrorBytes)
	}
	m.mu.Unlock()
	m.repairActivePage(owner)
	m.emitThreadState(owner)
	m.syncPanePresentation(owner)
	if closed {
		return nil, errClosedWhileCreating
	}
	if user {
		return nil, &shownOnTab{err: err}
	}
	return nil, err
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
			if c.p.owner == threadID {
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
	return m.closePage(ctx, access, pageID, true)
}

// CloseCompanionPage is the user closing a tab: it returns once the tab is
// gone from the state, and a failure of the teardown after it arrives as a
// "teardown" event.
func (m *Manager) CloseCompanionPage(ctx context.Context, access Access, pageID string) error {
	return m.closePage(ctx, access, pageID, false)
}

// closePage closes a live, closing, pending or suspended page. A page whose
// close is already running answers that close, so repeated closes agree.
func (m *Manager) closePage(ctx context.Context, access Access, pageID string, wait bool) error {
	pageID = strings.TrimSpace(pageID)
	c, err := m.beginCloseLive(access, pageID)
	if err == nil {
		if !wait {
			return nil
		}
		return c.wait(ctx)
	}
	if !errors.Is(err, errPageNotFound) {
		return err
	}
	if done, ok := m.closePending(access, pageID); ok {
		if !wait {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.forgetRecord(access.ThreadID, pageID)
}

// closeLivePage closes a live page and waits for its teardown. It answers
// errPageNotFound when the page is not live or closing.
func (m *Manager) closeLivePage(ctx context.Context, access Access, pageID string) error {
	c, err := m.beginCloseLive(access, pageID)
	if err != nil {
		return err
	}
	return c.wait(ctx)
}

// beginCloseLive starts the close of the caller's live page, or answers the
// close already running for it.
func (m *Manager) beginCloseLive(access Access, pageID string) (*pageClose, error) {
	m.mu.Lock()
	for _, scope := range m.scopes {
		if c := scope.closing[pageID]; c != nil {
			m.mu.Unlock()
			if c.p.owner != access.ThreadID {
				return nil, errPageNotFound
			}
			return c, nil
		}
	}
	m.mu.Unlock()
	p, scope, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		return nil, err
	}
	return m.closeFoundPage(p, scope)
}

// closeFoundPage closes a page a lookup found live: it leaves the tab set at
// once and is disposed after. It answers errPageNotFound when the page
// stopped being live meanwhile: suspended, it is now a record for ClosePage
// to forget; closing, its close answers.
func (m *Manager) closeFoundPage(p *managedPage, scope *workspaceScope) (*pageClose, error) {
	m.mu.Lock()
	if scope.pages[p.id] != p {
		m.mu.Unlock()
		return nil, errPageNotFound
	}
	delete(scope.pages, p.id)
	c := &pageClose{p: p, done: make(chan struct{})}
	if scope.closing == nil {
		scope.closing = make(map[string]*pageClose)
	}
	scope.closing[p.id] = c
	m.mu.Unlock()
	// An operation holding the page's lock gives it up; one waiting for it
	// finds the page gone (pageLive) or its operations ended.
	p.cancelOps()
	m.repairActivePage(p.owner)
	m.emitThreadState(p.owner)
	// Hides the page (presentPanes hides closing pages) before its engine
	// teardown, which can take a while.
	m.syncPanePresentation(p.owner)
	if !m.goLife(func() { m.runClose(scope, c) }) {
		m.runClose(scope, c)
	}
	return c, nil
}

// runClose is a close's engine half. It waits for the page's lock, closes
// the driver, and releases the scope, disposing it when this was its last
// page.
func (m *Manager) runClose(scope *workspaceScope, c *pageClose) {
	p := c.p
	p.mu.Lock()
	m.cancelPageDownloads(p, scope)
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
// creation's tab. The returned channel closes when the creation has
// finished with it; false when pageID is no such tab of the caller.
func (m *Manager) closePending(access Access, pageID string) (<-chan struct{}, bool) {
	m.mu.Lock()
	pending := m.pending[pageID]
	if pending == nil || pending.p.owner != access.ThreadID {
		m.mu.Unlock()
		return nil, false
	}
	if pending.err != "" {
		delete(m.pending, pageID)
	} else if !pending.closed {
		pending.closed = true
		pending.cancel()
	}
	m.mu.Unlock()
	m.repairActivePage(access.ThreadID)
	m.emitThreadState(access.ThreadID)
	m.syncPanePresentation(access.ThreadID)
	return pending.done, true
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
func (m *Manager) abandonPage(scope *workspaceScope, p *managedPage) error {
	p.driver.Close()
	m.mu.Lock()
	scope.creating--
	release := m.releaseScopeLocked(scope)
	m.mu.Unlock()
	if release {
		return m.disposeScope(context.Background(), scope)
	}
	return nil
}

// releaseScopeLocked unregisters scope when it has no page, none closing and
// none being created, and reports whether the caller must dispose it; the
// workspace is then disposing until disposeScope ends. Deciding and
// unregistering under one m.mu hold means a concurrent createPage either
// reserved the scope first or waits for the disposal, and a scope that is no
// longer registered is never disposed again. The caller holds m.mu.
func (m *Manager) releaseScopeLocked(scope *workspaceScope) bool {
	if len(scope.pages) > 0 || len(scope.closing) > 0 || scope.creating > 0 || m.scopes[scope.workspace] != scope {
		return false
	}
	delete(m.scopes, scope.workspace)
	if m.disposing == nil {
		m.disposing = make(map[string]chan struct{})
	}
	m.disposing[scope.workspace] = make(chan struct{})
	return true
}

// disposeScope disposes a scope releaseScopeLocked unregistered, frees its
// workspace for a new profile, and starts the idle close once no scope
// remains.
func (m *Manager) disposeScope(ctx context.Context, scope *workspaceScope) error {
	err := scope.profile.Dispose(ctx)
	m.mu.Lock()
	if done := m.disposing[scope.workspace]; done != nil {
		delete(m.disposing, scope.workspace)
		close(done)
	}
	noScopes := len(m.scopes) == 0 && len(m.disposing) == 0
	m.mu.Unlock()
	if noScopes {
		m.scheduleIdleClose()
	}
	return err
}

// disposeReleased disposes a released scope off the caller, reporting a
// failure as the teardown of the page whose close released it.
func (m *Manager) disposeReleased(scope *workspaceScope, threadID, pageID string) {
	run := func() {
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
