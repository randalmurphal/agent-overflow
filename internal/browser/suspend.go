package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// Suspended pages. A page is suspended in two cases only: the session reaper
// ended its thread's idle provider session (SuspendThread), or the app shut
// down with it open (saveOpenPages, then a restart loads it). Suspending
// unloads the engine page; the page stays in its thread's tab set as a record
// with its id, address, title, label and tab position, and holds no engine
// resource. Nothing about a page or pane being hidden suspends it.
//
// The next touch restores it by loading its address in a new engine page with
// the same id (resolvePage): any page-scoped tool call on it, or the user
// presenting it in the companion pane. Listing pages and closing one is not a
// touch. A restore passes the navigation policy every navigation passes, and
// a restore that fails leaves the record suspended and reports the failure to
// the caller.

// restoreTimeout bounds one restore: starting the engine and a profile if
// needed, creating the page, and loading its address.
const restoreTimeout = 2 * operationTimeout

// suspendedPage is one suspended page. Every field is guarded by m.mu.
type suspendedPage struct {
	id, owner         string
	url, title, label string
	tabOrder          int64
	createdAt         int64
	lastUse           int64
	// restoring is the restore in flight, which concurrent touches wait for
	// rather than loading the page twice.
	restoring *restoreCall
}

func (r *suspendedPage) info() PageInfo {
	return PageInfo{ID: r.id, Label: r.label, URL: r.url, Title: r.title, Suspended: true}
}

// restoreCall is one restore of a suspended page. err is written before done
// closes.
type restoreCall struct {
	done chan struct{}
	err  error
}

// threadTab is one entry of a thread's tab set: a live page or a suspended
// one.
type threadTab struct {
	id      string
	info    PageInfo
	order   int64
	created int64
	lastUse int64
	live    *managedPage
	rec     *suspendedPage
}

// threadTabs is THE tab-strip order of a thread's pages, live and suspended:
// every surface that lists them (companion state, browser_pages, ambiguity
// errors, tab moves) uses it, so the UI, the tools and the errors never
// disagree about which tab is first.
func (m *Manager) threadTabs(threadID string) []threadTab {
	m.mu.Lock()
	tabs := m.threadTabsLocked(threadID)
	m.mu.Unlock()
	return tabs
}

// threadTabsLocked is threadTabs with m.mu held by the caller.
func (m *Manager) threadTabsLocked(threadID string) []threadTab {
	var tabs []threadTab
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == threadID {
				tabs = append(tabs, threadTab{
					id: p.id, info: p.cachedInfo(), order: p.tabOrder.Load(), created: p.createdAt,
					lastUse: p.lastUse.Load(), live: p,
				})
			}
		}
	}
	for _, rec := range m.suspended {
		if rec.owner == threadID {
			tabs = append(tabs, threadTab{
				id: rec.id, info: rec.info(), order: rec.tabOrder, created: rec.createdAt,
				lastUse: rec.lastUse, rec: rec,
			})
		}
	}
	sort.Slice(tabs, func(i, j int) bool {
		if tabs[i].order != tabs[j].order {
			return tabs[i].order < tabs[j].order
		}
		return tabs[i].created < tabs[j].created
	})
	return tabs
}

// hasTab reports whether pageID is one of the thread's pages, live or
// suspended.
func (m *Manager) hasTab(threadID, pageID string) bool {
	for _, tab := range m.threadTabs(threadID) {
		if tab.id == pageID {
			return true
		}
	}
	return false
}

// livePageLocked finds a live page by id in any scope. The caller holds m.mu.
func (m *Manager) livePageLocked(pageID string) (*managedPage, *workspaceScope) {
	for _, scope := range m.scopes {
		if p := scope.pages[pageID]; p != nil {
			return p, scope
		}
	}
	return nil, nil
}

// resolvePage answers the caller's live page with pageID, restoring it first
// when it is suspended. Concurrent touches of one suspended page share one
// restore. skipLoad restores the page blank, for a caller that navigates it
// next. A page another thread owns is not found.
func (m *Manager) resolvePage(ctx context.Context, access Access, pageID string, skipLoad bool) (*managedPage, *workspaceScope, error) {
	pageID = strings.TrimSpace(pageID)
	for {
		m.mu.Lock()
		if p, scope := m.livePageLocked(pageID); p != nil {
			m.mu.Unlock()
			if p.owner != access.ThreadID {
				return nil, nil, errPageNotFound
			}
			return p, scope, nil
		}
		rec := m.suspended[pageID]
		if rec == nil || rec.owner != access.ThreadID {
			m.mu.Unlock()
			return nil, nil, errPageNotFound
		}
		call := rec.restoring
		if call == nil {
			call = &restoreCall{done: make(chan struct{})}
			rec.restoring = call
			// The restore runs on its own bounded context, so a caller that
			// gives up does not fail the restore for the others waiting on it.
			go m.runRestore(access, rec, call, skipLoad)
		}
		m.mu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		if call.err != nil {
			return nil, nil, call.err
		}
		// Restored: the next pass finds the live page and rechecks it.
	}
}

func (m *Manager) runRestore(access Access, rec *suspendedPage, call *restoreCall, skipLoad bool) {
	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	err := m.restorePage(ctx, access, rec, skipLoad)
	cancel()
	m.mu.Lock()
	if rec.restoring == call {
		rec.restoring = nil
	}
	m.mu.Unlock()
	call.err = err
	close(call.done)
}

// restorePage replaces rec with a live page of the same id at rec's address.
// The page is registered, and the record dropped, in one m.mu hold and only
// once it is fully loaded; any failure leaves the record as it was.
func (m *Manager) restorePage(ctx context.Context, access Access, rec *suspendedPage, skipLoad bool) error {
	m.mu.Lock()
	target, title, label := rec.url, rec.title, rec.label
	createdAt, lastUse := rec.createdAt, rec.lastUse
	m.mu.Unlock()
	if skipLoad || target == "" || target == "about:blank" {
		target, title = "", ""
	}
	if target != "" && !m.navigationAllowed(access, target) {
		return fmt.Errorf("browser: page %s cannot be restored: %s is not allowed; navigate the page elsewhere or close it", rec.id, target)
	}

	p := newManagedPage(access)
	p.id, p.createdAt = rec.id, createdAt
	p.lastUse.Store(lastUse)
	p.info = PageInfo{ID: rec.id, URL: "about:blank", Label: label}
	m.startMu.Lock()
	scope, err := m.openPageLocked(ctx, access, p, false)
	m.startMu.Unlock()
	if err != nil {
		return fmt.Errorf("browser: restore page %s: %w", rec.id, err)
	}
	// The load runs outside startMu, as any navigation does; the scope stays
	// reserved for the page until it is registered or abandoned.
	if target != "" {
		p.setInfo(PageInfo{ID: rec.id, URL: target, Title: title})
		opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
		err := p.driver.Navigate(opCtx, target)
		if err == nil {
			var info PageInfo
			if info, err = m.pageInfo(opCtx, p); err == nil {
				p.setInfo(info)
			}
		}
		cancel()
		if err != nil {
			return errors.Join(fmt.Errorf("browser: restore page %s at %s: %w", rec.id, target, err), m.abandonPage(scope, p))
		}
	}

	m.mu.Lock()
	closing := m.closed
	current := !closing && m.suspended[rec.id] == rec
	if current {
		scope.creating--
		// The record's tab position may have moved while the page loaded.
		p.tabOrder.Store(rec.tabOrder)
		delete(m.suspended, rec.id)
		scope.pages[p.id] = p
	}
	m.mu.Unlock()
	if closing {
		// Close saved the record before this page loaded; it stays suspended
		// for the next boot.
		return errors.Join(fmt.Errorf("browser: restore page %s: the browser is shutting down", rec.id), m.abandonPage(scope, p))
	}
	if !current {
		// The page was closed, its thread deleted or archived, or the browser
		// turned off while it loaded.
		return errors.Join(fmt.Errorf("browser: restore page %s: %w", rec.id, errPageNotFound), m.abandonPage(scope, p))
	}
	m.pageChanged(p)
	if err := m.persistThreads([]string{p.owner}); err != nil {
		// The page is live; its saved copy still lists it as suspended, which
		// only brings it back as a suspended page after a crash.
		log.Printf("browser: restore page %s: update saved pages: %v", p.id, err)
	}
	return nil
}

// SuspendThread unloads the live pages the thread owns and keeps each as a
// suspended page in its tab set. The session reaper calls it when it ends
// the thread's idle provider session; nothing else does. The page a mounted
// pane is showing stays live: unloading it would only make the pane reload
// it. A thread with no page suspended is left as it was. The saved copy is
// updated, and a failure to write it is returned. A cancelled ctx (the app
// shutting down) stops before the next page, and shutdown saves the rest.
func (m *Manager) SuspendThread(ctx context.Context, threadID string) error {
	m.mu.Lock()
	presented := m.presentedPageLocked(threadID)
	m.mu.Unlock()
	var errs []error
	suspended := false
	for _, p := range m.ownedPages(threadID) {
		if p.id == presented {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("browser: suspend thread pages: %w", err))
			break
		}
		moved, err := m.suspendPage(ctx, p)
		suspended = suspended || moved
		if err != nil {
			errs = append(errs, err)
		}
	}
	if !suspended {
		return errors.Join(errs...)
	}
	m.repairActivePage(threadID)
	m.emitThreadState(threadID)
	if ctx.Err() == nil {
		m.syncPanePresentation(threadID)
	}
	errs = append(errs, m.persistThreads([]string{threadID}))
	return errors.Join(errs...)
}

// suspendPage replaces one live page with its record and reports whether it
// did. It waits for the page's in-flight operation, reads the page's current
// address, and moves it from its scope to the records in one m.mu hold, so a
// lookup finds it as one or the other. A page closed meanwhile is left
// closed; a page of a browser that began shutting down is left for Close to
// save; and a page whose records were cleared meanwhile (the browser turned
// off, or its site data cleared, which close it) records nothing.
func (m *Manager) suspendPage(ctx context.Context, p *managedPage) (bool, error) {
	m.mu.Lock()
	cleared := m.recordsCleared
	m.mu.Unlock()
	p.mu.Lock()
	info := p.cachedInfo()
	opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
	current, err := m.pageInfo(opCtx, p)
	cancel()
	if err == nil {
		info.URL, info.Title = current.URL, current.Title
	} else if p.ctx.Err() == nil {
		log.Printf("browser: suspend page %s: read its address: %v; keeping %s", p.id, err, info.URL)
	}
	m.mu.Lock()
	var scope *workspaceScope
	for _, candidate := range m.scopes {
		if candidate.pages[p.id] == p {
			scope = candidate
			break
		}
	}
	if scope == nil || m.closed || m.recordsCleared != cleared {
		m.mu.Unlock()
		p.mu.Unlock()
		return false, nil
	}
	delete(scope.pages, p.id)
	m.suspended[p.id] = recordOf(p, info)
	release := m.releaseScopeLocked(scope)
	m.mu.Unlock()
	m.cancelPageDownloads(p, scope)
	p.driver.Close()
	p.mu.Unlock()
	if release {
		return true, m.disposeScope(ctx, scope)
	}
	return true, nil
}

func recordOf(p *managedPage, info PageInfo) *suspendedPage {
	return &suspendedPage{
		id: p.id, owner: p.owner,
		url: info.URL, title: info.Title, label: info.Label,
		tabOrder: p.tabOrder.Load(), createdAt: p.createdAt, lastUse: p.lastUse.Load(),
	}
}

// saveOpenPages records every live page as suspended and writes the saved
// copy of each thread that has one, so a restart brings the pages back. Run
// by Close before the engine is torn down. It reads only AO's cached page
// state: no engine call, because shutdown may run on the engine's UI thread.
func (m *Manager) saveOpenPages() error {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.Lock()
	var threads []string
	seen := make(map[string]bool)
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			m.suspended[p.id] = recordOf(p, p.cachedInfo())
			if !seen[p.owner] {
				seen[p.owner] = true
				threads = append(threads, p.owner)
			}
		}
	}
	m.mu.Unlock()
	return m.writeRecordFilesLocked(threads)
}

// forgetRecord closes one suspended page: its record and its saved copy go.
func (m *Manager) forgetRecord(threadID, pageID string) error {
	m.mu.Lock()
	rec := m.suspended[strings.TrimSpace(pageID)]
	found := rec != nil && rec.owner == threadID
	if found {
		delete(m.suspended, rec.id)
	}
	m.mu.Unlock()
	if !found {
		return errPageNotFound
	}
	m.repairActivePage(threadID)
	m.emitThreadState(threadID)
	m.syncPanePresentation(threadID)
	return m.persistThreads([]string{threadID})
}

// forgetThreadRecords drops every suspended page of the thread and removes
// its saved copy.
func (m *Manager) forgetThreadRecords(threadID string) error {
	m.mu.Lock()
	found := false
	for id, rec := range m.suspended {
		if rec.owner == threadID {
			delete(m.suspended, id)
			found = true
		}
	}
	m.mu.Unlock()
	if found {
		m.repairActivePage(threadID)
		m.emitThreadState(threadID)
	}
	return m.persistThreads([]string{threadID})
}

// forgetAllRecords drops every suspended page and the saved copies.
func (m *Manager) forgetAllRecords() error {
	m.persistMu.Lock()
	m.mu.Lock()
	threads := make(map[string]bool)
	for _, rec := range m.suspended {
		threads[rec.owner] = true
	}
	m.suspended = make(map[string]*suspendedPage)
	m.recordsCleared++
	m.mu.Unlock()
	err := removeRecordDir(m.recordDir)
	m.persistMu.Unlock()
	for threadID := range threads {
		m.repairActivePage(threadID)
		m.emitThreadState(threadID)
	}
	return err
}
