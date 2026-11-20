package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
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
// the same id (resolvePage, restore.go): any page-scoped tool call on it, or
// the user presenting it in the companion pane. Listing pages and closing one
// is not a touch. A restore passes the navigation policy every navigation
// passes, and a restore that fails leaves the record suspended and reports
// the failure to the caller.

// suspendedPage is one suspended page. Every field is guarded by m.mu.
type suspendedPage struct {
	id PageID
	// owner is the thread lifetime the record is a tab of.
	owner             *threadBrowser
	url, title, label string
	tabOrder          int64
	createdAt         int64
	lastUse           int64
	// restoring is the restore in flight, which concurrent touches wait for
	// rather than loading the page twice.
	restoring *restoreCall
	// failure is why the page stopped when its process ended (crash.go),
	// "" for a page suspended on purpose. Such a tab is restored only when
	// asked to (ReloadCompanionPage) or used by a tool, never by selecting
	// it. It is not saved: after a restart the tab is an ordinary suspended
	// one.
	failure string
}

func (r *suspendedPage) info() PageInfo {
	return PageInfo{ID: string(r.id), Label: r.label, URL: r.url, Title: r.title, Suspended: true}
}

// SuspendThread unloads the live pages the thread owns and keeps each as a
// suspended page in its tab set. The session reaper calls it when it ends
// the thread's idle provider session; nothing else does. The page a mounted
// pane is showing stays live: unloading it would only make the pane reload
// it. A thread with no page suspended is left as it was. The saved copy is
// updated, and a failure to write it is returned. A cancelled ctx (the app
// shutting down) stops before the next page, and shutdown saves the rest.
//
// The reaper names the thread by id; the lifetime current then is the one
// whose pages it suspends.
func (m *Manager) SuspendThread(ctx context.Context, threadID ThreadID) error {
	m.mu.Lock()
	tb := m.currentLocked(threadID)
	presented := m.presentedPageLocked(tb)
	m.mu.Unlock()
	if tb == nil {
		return nil
	}
	var errs []error
	suspended := false
	for _, p := range m.ownedPages(tb) {
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
	m.repairActivePage(tb)
	m.emitThreadState(threadID)
	if ctx.Err() == nil {
		m.syncPanePresentation(threadID)
	}
	errs = append(errs, m.persistThreads([]ThreadID{threadID}))
	return errors.Join(errs...)
}

// suspendPage replaces one live page with its record and reports whether it
// did. It waits for the page's in-flight operation, reads the page's current
// address, and moves it from its scope to the records in one m.mu hold, so a
// lookup finds it as one or the other. A page closed meanwhile is left
// closed; a page a mounted pane shows by then is left live; a page of a
// browser that began shutting down is left for Close to save; a page a
// forget of every page (the browser turned off, or its site data cleared)
// or its thread's close is closing records nothing. The move waits for a
// jar switch of the thread writing its saved copy.
func (m *Manager) suspendPage(ctx context.Context, p *managedPage) (bool, error) {
	p.mu.Lock()
	opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
	read, err := m.settledAddress(opCtx, p)
	cancel()
	info := p.cachedInfo()
	if err == nil {
		// The page's address as of its suspension, read under the page lock
		// with no report landing during the read (settledAddress).
		info.URL = truncateUTF8(read.URL, maxBrowserURLBytes)
		info.Title = truncateUTF8(read.Title, maxBrowserTitleBytes)
	} else if p.ctx.Err() == nil {
		log.Printf("browser: suspend page %s: read its address: %v; keeping %s", p.id, err, info.URL)
	}
	if err := m.lockStructure(ctx, p.owner); err != nil {
		p.mu.Unlock()
		return false, err
	}
	var scope *jarScope
	for _, candidate := range m.scopes {
		if candidate.pages[p.id] == p {
			scope = candidate
			break
		}
	}
	// A page a mounted pane came to show meanwhile stays live, as it would
	// have had it been shown from the start (SuspendThread).
	if scope == nil || !m.recordsAdmittedLocked(p.owner) || m.presentedPageLocked(p.owner) == p.id {
		m.mu.Unlock()
		p.mu.Unlock()
		return false, nil
	}
	ended := m.pageLeftLocked(scope, p, "page suspended")
	m.suspended[p.id] = recordOf(p, info)
	release := m.releaseScopeLocked(scope)
	m.mu.Unlock()
	m.publishDownloads(ended.notes...)
	m.stopEndedDownloads(ended)
	p.driver.Close()
	m.endFindLocked(p)
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
	var threads []ThreadID
	seen := make(map[ThreadID]bool)
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			m.suspended[p.id] = recordOf(p, p.cachedInfo())
			if !seen[p.owner.id] {
				seen[p.owner.id] = true
				threads = append(threads, p.owner.id)
			}
		}
	}
	m.mu.Unlock()
	return m.writeRecordFilesLocked(threads)
}

// forgetRecordLocked closes one suspended page: it drops tb's record of
// pageID, answering whether it had one, and the caller, which holds m.mu,
// calls recordForgotten after releasing it, so the saved copy goes too.
func (m *Manager) forgetRecordLocked(tb *threadBrowser, pageID PageID, byUser bool) bool {
	rec := m.suspended[pageID.trim()]
	if rec == nil || rec.owner != tb {
		return false
	}
	if byUser {
		m.recordUserCloseLocked(tb, rec.id)
	}
	m.dropRecordLocked(rec)
	return true
}

// recordForgotten publishes a forgotten record of tb and saves the thread's
// records.
func (m *Manager) recordForgotten(tb *threadBrowser) error {
	m.repairActivePage(tb)
	m.emitThreadState(tb.id)
	m.syncPanePresentation(tb.id)
	return m.persistThreads([]ThreadID{tb.id})
}

// dropRecordLocked forgets rec and cancels its restore in flight, which
// could only be refused at registration. The caller holds m.mu.
func (m *Manager) dropRecordLocked(rec *suspendedPage) {
	delete(m.suspended, rec.id)
	if rec.restoring != nil {
		rec.restoring.cancel()
	}
}

// forgetAllRecords drops every suspended page and the saved copies. keepJars
// keeps the threads' jar settings, saved again on their own; otherwise every
// thread's browser returns to the default jar.
func (m *Manager) forgetAllRecords(keepJars bool) error {
	m.persistMu.Lock()
	m.mu.Lock()
	threads := make(map[*threadBrowser]bool)
	for _, rec := range m.suspended {
		threads[rec.owner] = true
		m.dropRecordLocked(rec)
	}
	var jarThreads []ThreadID
	for threadID, tb := range m.threads {
		switch {
		case jarKindOf(tb.session) == JarWorktree:
		case keepJars:
			jarThreads = append(jarThreads, threadID)
		default:
			tb.session.Jar = JarWorktree
		}
	}
	m.mu.Unlock()
	err := removeRecordDir(m.recordDir)
	if err == nil && len(jarThreads) > 0 {
		err = m.writeRecordFilesLocked(jarThreads)
	}
	m.persistMu.Unlock()
	for tb := range threads {
		m.repairActivePage(tb)
		m.emitThreadState(tb.id)
	}
	return err
}
