package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// Restoring a suspended page (suspend.go) on its next touch: every path
// that acts on a page by id resolves it here (resolvePage), and concurrent
// touches of one record share one restore.

// restoreTimeout bounds one restore: starting the engine and a profile if
// needed, creating the page, and loading its address.
const restoreTimeout = 2 * operationTimeout

// restoreCall is one restore of a suspended page. err is written before done
// closes. cancel ends the restore; forgetting its record calls it
// (dropRecordLocked).
type restoreCall struct {
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

// errJarTeardown refuses a restore's admission while its thread's jar switch
// is still tearing pages down (jarTeardownLocked); the restore waits and
// tries again.
var errJarTeardown = errors.New("browser: the cookie jar switch is still closing pages")

// resolvePage answers the caller's live page with pageID, restoring it first
// when it is suspended. Concurrent touches of one suspended page share one
// restore. skipLoad restores the page blank, for a caller that navigates it
// next. A page of another thread, or of another lifetime of the caller's,
// is not found.
func (m *Manager) resolvePage(ctx context.Context, access Access, pageID PageID, skipLoad bool) (*managedPage, *jarScope, error) {
	pageID = pageID.trim()
	for {
		m.mu.Lock()
		if p, scope := m.livePageLocked(pageID); p != nil {
			m.mu.Unlock()
			if p.owner != access.tb {
				return nil, nil, errPageNotFound
			}
			return p, scope, nil
		}
		rec := m.suspended[pageID]
		if rec == nil || rec.owner != access.tb {
			m.mu.Unlock()
			return nil, nil, errPageNotFound
		}
		call := rec.restoring
		started := call == nil
		if started {
			// The restore runs on its own bounded context, so a caller that
			// gives up does not fail the restore for the others waiting on it.
			restoreCtx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
			call = &restoreCall{done: make(chan struct{}), cancel: cancel}
			rec.restoring = call
			go m.runRestore(restoreCtx, access, rec, call, skipLoad)
		}
		m.mu.Unlock()
		if started {
			// The tab shows it is restoring (PageInfo.Restoring).
			m.emitThreadState(access.ThreadID)
		}
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

func (m *Manager) runRestore(ctx context.Context, access Access, rec *suspendedPage, call *restoreCall, skipLoad bool) {
	err := m.restorePage(ctx, access, rec, skipLoad)
	call.cancel()
	m.mu.Lock()
	if rec.restoring == call {
		rec.restoring = nil
	}
	forgotten := m.suspended[rec.id] != rec
	m.mu.Unlock()
	if err != nil && forgotten && !errors.Is(err, errPageNotFound) && !errors.Is(err, ErrThreadClosed) {
		// Forgetting the record cancelled the restore (dropRecordLocked): the
		// tab is gone, not failed. What the cancelled restore reported, its
		// page's cleanup included, is logged.
		log.Printf("browser: restore page %s ended with its tab: %v", rec.id, err)
		err = fmt.Errorf("browser: restore page %s: %w", rec.id, errPageNotFound)
	}
	call.err = err
	close(call.done)
	if err != nil {
		// A restored page was announced with its registration; a tab left
		// suspended stops showing the restore.
		m.emitThreadState(rec.owner.id)
	}
}

// restorePage replaces rec with a live page of the same id at rec's address,
// in its thread's jar. The page is registered, and the record
// dropped, in one m.mu hold and only once it is fully loaded; any failure
// leaves the record as it was. A page loaded in a jar its thread switched
// away from meanwhile is closed and the restore starts again in the new
// jar.
//
// Each attempt waits for its thread's jar teardown (awaitJarTeardown) and
// is admitted by openPageLocked only while rec is still the page's record
// and no newer switch has teardown pending (restoreAdmissionLocked). The
// check runs under m.mu, which forgetting a record holds as it cancels the
// restore (dropRecordLocked), so a restore whose record is gone never
// creates its page. One admitted first holds startMu until its page exists,
// so clearing site data or turning the browser off closes that page with the
// browser (closeBrowser) before removing any data.
func (m *Manager) restorePage(ctx context.Context, access Access, rec *suspendedPage, skipLoad bool) error {
	for {
		if err := m.awaitJarTeardown(ctx, rec.owner); err != nil {
			return fmt.Errorf("browser: restore page %s: %w", rec.id, err)
		}
		m.mu.Lock()
		target, title, label := rec.url, rec.title, rec.label
		createdAt, lastUse := rec.createdAt, rec.lastUse
		m.mu.Unlock()
		if skipLoad || target == "" || target == "about:blank" {
			target, title = "", ""
		}
		if target != "" && !m.navigationAllowed(access.Caller(), target) {
			return fmt.Errorf("browser: page %s cannot be restored: %s is not allowed; navigate the page elsewhere or close it", rec.id, target)
		}

		p := newManagedPage(access.tb, access.Caller())
		p.id, p.createdAt = rec.id, createdAt
		p.lastUse.Store(lastUse)
		p.info = PageInfo{ID: string(rec.id), URL: "about:blank", Label: label}
		scope, err := m.openAndLoad(ctx, access.Caller(), p, target, title, func() error { return m.restoreAdmissionLocked(access, rec) })
		if errors.Is(err, errJarTeardown) {
			continue
		}
		if err != nil {
			return fmt.Errorf("browser: restore page %s: %w", rec.id, err)
		}

		// The commit waits for a jar switch writing the thread's saved copy:
		// a restore is a change of the tabs it captured.
		if err := m.lockStructure(ctx, access.tb); err != nil {
			return errors.Join(fmt.Errorf("browser: restore page %s: %w", rec.id, err), m.abandonPage(scope, p))
		}
		closing := m.closed
		refused := access.admitLocked()
		current := !closing && refused == nil && m.suspended[rec.id] == rec
		stale := current && !m.jarCurrentLocked(p)
		if current && !stale {
			delete(scope.creating, p)
			// The record's tab position may have moved while the page loaded.
			p.tabOrder.Store(rec.tabOrder)
			delete(m.suspended, rec.id)
			m.listPageLocked(scope, p)
		}
		m.mu.Unlock()
		if closing {
			// Close saved the record before this page loaded; it stays suspended
			// for the next boot.
			return errors.Join(fmt.Errorf("browser: restore page %s: the browser is shutting down", rec.id), m.abandonPage(scope, p))
		}
		if refused != nil {
			// The thread's pages were closed (deleted, archived, handed off)
			// while the page loaded.
			return errors.Join(fmt.Errorf("browser: restore page %s: %w", rec.id, refused), m.abandonPage(scope, p))
		}
		if !current {
			// The page was closed or the browser turned off while it loaded.
			return errors.Join(fmt.Errorf("browser: restore page %s: %w", rec.id, errPageNotFound), m.abandonPage(scope, p))
		}
		if stale {
			m.abandonStale(scope, p, p)
			continue
		}
		// The tab leaves its suspended state.
		m.pageAdded(p)
		if err := m.persistThreads([]ThreadID{p.owner.id}); err != nil {
			// The page is live; its saved copy still lists it as suspended, which
			// only brings it back as a suspended page after a crash.
			log.Printf("browser: restore page %s: update saved pages: %v", p.id, err)
		}
		return nil
	}
}

// restoreAdmissionLocked admits a restore of rec while the lifetime access
// is bound to holds, rec is still its page's record and its thread has no
// jar teardown pending. The caller holds m.mu.
func (m *Manager) restoreAdmissionLocked(access Access, rec *suspendedPage) error {
	if err := access.admitLocked(); err != nil {
		return err
	}
	if m.suspended[rec.id] != rec {
		return errPageNotFound
	}
	if len(jarTeardownLocked(rec.owner)) > 0 {
		return errJarTeardown
	}
	return nil
}

// jarTeardownLocked answers tb's jar teardowns still pending (SetJar) and
// forgets the finished ones. The caller holds m.mu.
func jarTeardownLocked(tb *threadBrowser) []<-chan struct{} {
	var pending []<-chan struct{}
	for _, done := range tb.session.jarTeardown {
		select {
		case <-done:
		default:
			pending = append(pending, done)
		}
	}
	tb.session.jarTeardown = pending
	return pending
}

// awaitJarTeardown waits until the pages tb's jar switches closed have
// finished their teardown, their old profiles' disposal included, so a
// restore neither shares an id with a page still closing nor meets a cap
// only those pages hold.
func (m *Manager) awaitJarTeardown(ctx context.Context, tb *threadBrowser) error {
	for {
		m.mu.Lock()
		pending := jarTeardownLocked(tb)
		m.mu.Unlock()
		if len(pending) == 0 {
			return nil
		}
		for _, done := range pending {
			select {
			case <-done:
			case <-ctx.Done():
				return fmt.Errorf("the pages its cookie jar switch closed are still closing: %w", ctx.Err())
			}
		}
	}
}

// openAndLoad creates p's engine page (openPageLocked, which runs admit) and
// loads target in it when target is set, with title shown while it loads.
// The load runs outside startMu, as any navigation does; the scope stays
// reserved for the page until the caller registers or abandons it. A failed
// load abandons the page.
func (m *Manager) openAndLoad(ctx context.Context, access Caller, p *managedPage, target, title string, admit func() error) (*jarScope, error) {
	m.startMu.Lock()
	scope, err := m.openPageLocked(ctx, access, p, admit)
	m.startMu.Unlock()
	if err != nil || target == "" {
		return scope, err
	}
	// The page is in no thread's tabs yet, so no operation reaches it and no
	// engine report finds it (pageByHandleLocked): the load's read is the
	// only commit, and the registration push carries it.
	p.commitInfo(pageUpdate{Location: &pageLocation{URL: target, Title: title}})
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	err = p.driver.Navigate(opCtx, target)
	if err == nil {
		_, _, err = m.readInfo(opCtx, p, pageListing{})
	}
	cancel()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("load %s: %w", target, err), m.abandonPage(scope, p))
	}
	return scope, nil
}
