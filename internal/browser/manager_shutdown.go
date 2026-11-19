package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// CloseThread ends the thread's browser lifetime (thread_life.go): every
// tab it has, in every state, its session, browsing history, pane color and
// download list. In its first hold it detaches the lifetime from the
// registry, drops its suspended pages and ends the order of every pane mount
// attached to it, so from then an action bound to it changes nothing
// (ErrThreadClosed), a numbered command of its mount passes, and a bind
// begins a new, empty lifetime the cleanup below never reaches. It then
// saves the thread's copy as its current lifetime is, which removes it
// unless a new lifetime saved tabs meanwhile, and closes what it detached:
// the tabs still being created and the live pages, a popup listed before
// the detach included. A page the engine closed meanwhile counts as closed.
// A workspace profile that fails to dispose once its last page is gone (a
// browser process that would not stop, ephemeral site data left on disk) is
// returned, as ClosePage returns it, after every page is closed; the profile
// is no longer registered, so a second close does not repeat the failure. A
// saved copy that cannot be written is logged instead: its pages are gone
// from memory, and the next boot removes the saved copy of a deleted or
// archived thread.
func (m *Manager) CloseThread(ctx context.Context, threadID ThreadID) error {
	// persistMu first, so the detach and the save that follows are one step
	// in the order of the thread's saves: a save of a new lifetime is
	// ordered after this one.
	m.persistMu.Lock()
	m.mu.Lock()
	tb := m.detachLocked(threadID)
	if tb != nil {
		// The lifetime's browsing goes with it: a cache of the thread's
		// recent addresses reads the forget generation (forgetBrowsingLocked).
		m.recentGen++
		for _, rec := range m.suspended {
			if rec.owner == tb {
				m.dropRecordLocked(rec)
			}
		}
		for _, mount := range m.panes {
			if mount.owner == tb && mount.order != nil {
				mount.order.End()
			}
		}
	}
	m.mu.Unlock()
	if err := m.writeRecordFilesLocked([]ThreadID{threadID}); err != nil {
		log.Printf("browser: close thread %s: %v", threadID, err)
	}
	m.persistMu.Unlock()
	if tb == nil {
		return nil
	}
	m.emitThreadState(threadID)
	m.closeThreadPending(tb)
	var errs []error
	for pages := m.ownedPages(tb); len(pages) > 0; pages = m.ownedPages(tb) {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, fmt.Errorf("browser: close thread pages: %w", err))...)
		}
		for _, p := range pages {
			if err := m.closeLivePage(ctx, tb, p.id); err != nil && !errors.Is(err, errPageNotFound) {
				errs = append(errs, fmt.Errorf("browser: close thread %s: %w", threadID, err))
			}
		}
	}
	m.forgetThreadDownloads(tb)
	return errors.Join(errs...)
}

// ClearSiteData closes every engine page first, then deletes the site data
// (spec §4). The order is load-bearing: an engine still holding a profile open
// would write its cookie jar back out over the cleared state. One startMu hold
// spans the browser's close and both clears, so no page opens a profile
// against the old site data before it is gone.
//
// Two halves, because two kinds of engine exist: the AO-owned profile tree is
// deleted here (WebKitGTK keeps its data under it), and an engine whose data
// lives somewhere this process cannot reach by path — the launcher's WebView2
// user-data folder, WebKit's own macOS data-store directory — implements
// engineSiteData and clears its own. Both halves run; a Settings button that
// silently clears nothing on some platforms is not an option.
// The recent addresses and closed tabs go with the pages (forgetAllPages).
func (m *Manager) ClearSiteData(ctx context.Context) error {
	return m.forgetAllPages(ctx, true, func() error {
		var errs []error
		if err := os.RemoveAll(m.profileDir); err != nil {
			errs = append(errs, fmt.Errorf("browser: clear site data: %w", err))
		}
		if engine, ok := m.engine.(engineSiteData); ok {
			if err := engine.ClearSiteData(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}

// forgetAllPages closes every page, suspended ones and their saved copies
// included, stops the engine, and then forgets every thread's recent
// addresses and closed tabs: an open still running then has no live page to
// record on (recordRecentAddress), and a close the user made meanwhile is
// forgotten too. keepJars keeps the threads' jar settings. clear, when set,
// runs once the browser closed without error, in the startMu hold of that
// close, so no page opens before it ends.
//
// No page becomes a record from the start of the forget to its end
// (beginForget): a live page a jar switch, the reaper or a crash would
// suspend is one the close takes, and the records are forgotten once, before
// the engine start is interrupted, which cancels the restores in flight.
func (m *Manager) forgetAllPages(ctx context.Context, keepJars bool, clear func() error) error {
	m.beginForget()
	err := m.forgetAllRecords(keepJars)
	// Release an in-flight engine start first: it holds startMu.
	m.engine.Interrupt()
	m.startMu.Lock()
	err = errors.Join(err, m.closeBrowserLocked(ctx))
	if err == nil && clear != nil {
		err = clear()
	}
	m.mu.Lock()
	threads := m.forgetAllBrowsingLocked()
	m.forgetting--
	m.mu.Unlock()
	m.startMu.Unlock()
	// The closes pushed state before the forget; these carry its generation.
	for _, threadID := range threads {
		m.emitThreadState(threadID)
	}
	return err
}

// beginForget starts a forget of every page: from here until forgetAllPages
// ends it, no page becomes a record (forgettingLocked). It waits for the
// record writes in flight, which hold persistMu from their write to their
// commit, so none commits after it.
func (m *Manager) beginForget() {
	m.persistMu.Lock()
	m.mu.Lock()
	m.forgetting++
	m.mu.Unlock()
	m.persistMu.Unlock()
}

// forgettingLocked reports whether a forget of every page runs, during which
// no page becomes a record. The caller holds m.mu.
func (m *Manager) forgettingLocked() bool { return m.forgetting > 0 }

// Close shuts the manager down. The pages open at that moment are saved as
// suspended pages first, so they come back in their threads after a restart.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	err := errors.Join(m.saveOpenPages(), m.closeBrowser(context.Background()))
	// The browser's close disposed every profile, closing pages included; a
	// close or disposal still running finishes against it. On the engine's
	// UI thread it cannot be waited for: its engine calls dispatch to this
	// very thread, and the engine is already stopped.
	m.lifeGate.Lock()
	m.lifeClosed = true
	m.lifeGate.Unlock()
	if ui, ok := m.engine.(engineUIThread); !ok || !ui.OnUIThread() {
		m.lifeWork.Wait()
		// A finish the engine reported before it stopped reaches its
		// publish; its events may dispatch to the UI thread, so not there.
		if events, ok := m.engine.(engineDownloadEvents); ok {
			events.WaitDownloadEvents()
		}
	}
	// A publish in flight completes, so a finished download is in the
	// Downloads folder and its row settled; one still queued on the UI
	// thread's path stays staged, never partly public.
	m.waitPublishes()
	// The browser's close submitted the end of every page's find; what an
	// engine reports after it has no page left to end a find on.
	m.findGate.Lock()
	m.findClosed = true
	m.findGate.Unlock()
	m.findWork.Wait()
	return err
}

func (m *Manager) closeBrowser(caller context.Context) error {
	// Release an in-flight engine start first: it holds startMu, and this
	// shutdown needs it.
	m.engine.Interrupt()
	m.startMu.Lock()
	defer m.startMu.Unlock()
	return m.closeBrowserLocked(caller)
}

// closeIdleBrowser is the idle timer's close. It decides under startMu, which
// createPage holds from the engine start to the page's registration, so a
// page being created keeps the engine running.
func (m *Manager) closeIdleBrowser() {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	idle := len(m.scopes) == 0 && len(m.disposing) == 0
	m.mu.Unlock()
	if !idle {
		return
	}
	if err := m.closeBrowserLocked(context.Background()); err != nil {
		log.Printf("browser: idle close: %v", err)
	}
}

// closeBrowserLocked disposes every profile and stops the engine. The caller
// holds startMu.
func (m *Manager) closeBrowserLocked(caller context.Context) error {
	m.mu.Lock()
	scopes := make([]*jarScope, 0, len(m.scopes))
	owners := make(map[*threadBrowser]struct{})
	var pages []*managedPage
	var ended []endedDownloads
	for _, scope := range m.scopes {
		scopes = append(scopes, scope)
		for _, p := range scope.pages {
			owners[p.owner] = struct{}{}
			pages = append(pages, p)
			ended = append(ended, m.pageLeftLocked(scope, p, "the browser closed"))
		}
	}
	m.scopes = make(map[jarKey]*jarScope)
	m.forgetAuthoritiesLocked()
	// A failed creation's tab names a page that never existed in the
	// browser being closed.
	for id, pending := range m.pending {
		if pending.err != "" {
			delete(m.pending, id)
			owners[pending.p.owner] = struct{}{}
		}
	}
	disposing := make([]chan struct{}, 0, len(m.disposing))
	for _, done := range m.disposing {
		disposing = append(disposing, done)
	}
	for owner := range owners {
		owner.session.ActivePageID = ""
		owner.session.Visible = false
		owner.session.UpdatedAt = time.Now()
	}
	// Every private jar goes with the browser; the next private page opens
	// a new one.
	for _, tb := range m.threads {
		tb.session.privateJar = ""
	}
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	m.mu.Unlock()
	// A popup not listed yet goes with the browser, its downloads with it;
	// their files go once the engine writing them has stopped.
	parked := m.dropParked()
	for _, p := range pages {
		m.endFind(p)
	}
	for _, e := range ended {
		m.publishDownloads(e.notes...)
		m.stopEndedDownloads(e)
	}
	for owner := range owners {
		m.emitThreadState(owner.id)
	}
	closeCtx, closeCancel := context.WithTimeout(caller, 20*time.Second)
	defer closeCancel()
	errs := make(chan error, len(scopes))
	done := make(chan struct{})
	if ui, ok := m.engine.(engineUIThread); ok && ui.OnUIThread() {
		// The caller is the UI thread itself (Wails runs ServiceShutdown on it
		// and blocks it). Every native call below is a dispatch to that thread:
		// inline from here, a deadlock-until-timeout from a goroutine. So
		// sequential, on this thread, bounded by the engine and not the clock.
		for _, scope := range scopes {
			if err := scope.profile.Dispose(closeCtx); err != nil {
				errs <- err
			}
		}
		close(done)
	} else {
		var wg sync.WaitGroup
		for _, scope := range scopes {
			wg.Go(func() {
				if err := scope.profile.Dispose(closeCtx); err != nil {
					errs <- err
				}
			})
		}
		go func() { wg.Wait(); close(done) }()
	}
	timedOut := false
	select {
	case <-done:
	case <-closeCtx.Done():
		timedOut = true
	}
	if ui, ok := m.engine.(engineUIThread); !ok || !ui.OnUIThread() {
		// A profile released by a page's close is disposed off that close;
		// the engine stops after it. On the UI thread that disposal waits for
		// this very thread, so the stop goes first there.
		for _, wait := range disposing {
			select {
			case <-wait:
			case <-closeCtx.Done():
				timedOut = true
			}
		}
	}
	m.engine.Stop()
	if timedOut {
		// Engine teardown releases any command that was still waiting.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	for _, discard := range parked {
		m.finishDiscard(discard)
	}
	joined := make([]error, 0, len(errs)+1)
	for len(errs) > 0 {
		err := <-errs
		joined = append(joined, err)
	}
	if timedOut {
		joined = append(joined, closeCtx.Err())
	} else if caller.Err() != nil {
		joined = append(joined, caller.Err())
	}
	return errors.Join(joined...)
}

func (m *Manager) scheduleIdleClose() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(idleBrowserDelay, m.closeIdleBrowser)
}
