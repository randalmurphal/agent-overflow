package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// SetJar sets the cookie jar the thread's pages open in (jar.go) and saves
// the setting with the thread's pages, which is the jar they restore into.
// With no live page it just applies.
// Otherwise each live page closes and its address reopens in the new jar as a
// new tab in the same position, the active tab staying active. Page state
// (forms, history, sign-ins kept in the old jar) does not come along; the
// pane asks before it switches. A suspended tab stays suspended and opens in
// the new jar when it is next used.
//
// The switch advances the thread's jar generation in the same m.mu hold that
// moves its live pages to closing, so every admission still in flight (a
// create, a restore, a popup) finds itself stale and lands in the new jar or
// nowhere (jarCurrentLocked). A reopened tab takes its predecessor's place in
// the thread's cap and creates its engine page once the predecessor's
// teardown has ended, so no cap is exceeded while both exist. A tab whose
// reopen fails stays with its error, as a failed new tab does.
func (m *Manager) SetJar(access Access, kind string) error {
	kind = strings.TrimSpace(kind)
	if !validJarKind(kind) {
		return fmt.Errorf("browser: unknown cookie jar %q", truncateUTF8(kind, 32))
	}
	if strings.TrimSpace(access.ThreadID) == "" || strings.TrimSpace(access.Workspace) == "" {
		return fmt.Errorf("browser: invalid caller scope")
	}
	if kind == JarProject && strings.TrimSpace(access.ProjectID) == "" {
		return fmt.Errorf("browser: this thread has no project to share a cookie jar with")
	}
	threadID := access.ThreadID

	type reopen struct {
		scope   *jarScope
		close   *pageClose
		pending *pendingPage
		ctx     context.Context
		target  string
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("browser: manager closed")
	}
	info := m.sessionLocked(threadID)
	if jarKindOf(info) == kind {
		m.mu.Unlock()
		return nil
	}
	info.Jar, info.jarGen, info.privateJar = kind, info.jarGen+1, ""
	if m.pending == nil {
		m.pending = make(map[string]*pendingPage)
	}
	var reopens []reopen
	for _, tab := range m.threadTabsLocked(threadID) {
		old := tab.live
		if old == nil {
			continue
		}
		_, scope := m.livePageLocked(old.id)
		c := markClosingLocked(old, scope)
		c.replaced = true
		shown := old.cachedInfo()
		p := newManagedPage(access)
		p.createdAt = old.createdAt
		p.tabOrder.Store(old.tabOrder.Load())
		p.lastUse.Store(old.lastUse.Load())
		p.info = PageInfo{ID: p.id, URL: shown.URL, Title: shown.Title, Label: shown.Label}
		ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
		pending := &pendingPage{p: p, cancel: cancel, done: make(chan struct{})}
		m.pending[p.id] = pending
		if info.ActivePageID == old.id {
			info.ActivePageID = p.id
		}
		target := shown.URL
		if target == "about:blank" {
			target = ""
		}
		reopens = append(reopens, reopen{scope: scope, close: c, pending: pending, ctx: ctx, target: target})
	}
	info.UpdatedAt = time.Now()
	m.sessions[threadID] = info
	m.mu.Unlock()

	err := m.persistThreads([]string{threadID})
	for _, r := range reopens {
		m.startClose(r.scope, r.close)
		if !m.goLife(func() { m.reopenPage(r.ctx, access, r.pending, r.close.done, r.target) }) {
			// The manager is closing; the tab goes with the browser.
			r.pending.cancel()
			close(r.pending.done)
			m.dropPending(r.pending)
		}
	}
	m.emitThreadState(threadID)
	m.syncPanePresentation(threadID)
	return err
}

// reopenPage creates a tab a jar switch reopens, once the page it replaces
// has closed, in its thread's current jar, and loads target in it. It is
// registered only once loaded. A switch made meanwhile reopens it again in
// the newer jar; a close of its tab, or of the browser, drops it; any other
// failure stays on the tab as its error.
func (m *Manager) reopenPage(ctx context.Context, access Access, pending *pendingPage, after <-chan struct{}, target string) {
	defer close(pending.done)
	defer pending.cancel()
	owner := access.ThreadID
	var err error
	select {
	case <-after:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err == nil && target != "" && !m.navigationAllowed(access, target) {
		err = fmt.Errorf("browser: %s is not allowed", target)
	}
	for err == nil {
		p := pending.p
		var scope *jarScope
		if scope, err = m.openAndLoad(ctx, access, p, target, p.cachedInfo().Title); err != nil {
			break
		}
		m.mu.Lock()
		dropped := pending.closed || m.scopes[scope.jar.key] != scope
		stale := !dropped && !m.jarCurrentLocked(p)
		var fresh *managedPage
		switch {
		case stale:
			fresh = m.freshPendingPageLocked(pending)
		case !dropped:
			delete(m.pending, p.id)
			delete(scope.creating, p)
			scope.pages[p.id] = p
		}
		m.mu.Unlock()
		switch {
		case stale:
			m.abandonStale(scope, p, fresh)
			continue
		case dropped:
			pending.cleanupErr = m.abandonPage(scope, p)
			m.dropPending(pending)
			if pending.cleanupErr != nil {
				m.reportTeardown(owner, p.id, pending.cleanupErr)
			}
			m.repairActivePage(owner)
			m.emitThreadState(owner)
			m.syncPanePresentation(owner)
			return
		}
		m.pageChanged(p)
		return
	}

	m.mu.Lock()
	closed := pending.closed
	if closed {
		delete(m.pending, pending.p.id)
	} else {
		pending.err = truncateUTF8(fmt.Sprintf("could not reopen in the new cookie jar: %v", err), maxPageErrorBytes)
	}
	m.mu.Unlock()
	// As for a user's failed tab (createPage): the profile the failure left
	// behind failing to dispose is reported whether or not a close has taken
	// the tab, since one can at any point from here and then answers with it.
	pending.cleanupErr = teardownPart(err)
	if pending.cleanupErr != nil {
		m.reportTeardown(owner, pending.p.id, pending.cleanupErr)
	}
	if !closed && !errors.Is(err, context.Canceled) {
		log.Printf("browser: reopen page %s of thread %s in its new cookie jar: %v", pending.p.id, owner, err)
	}
	m.repairActivePage(owner)
	m.emitThreadState(owner)
	m.syncPanePresentation(owner)
}
