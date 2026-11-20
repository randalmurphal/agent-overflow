package browser

import (
	"errors"
	"log"

	"agent-overflow/internal/browser/ids"
)

// A page the Manager creates or restores can close or crash before it is
// listed, and its engine reports that by the page's handle, which the
// Manager learns only when the engine's NewPage returns. A popup has its
// admission for this (popupadmit); a created page has its creation end.
// The engine's report records the end in its own call (noteCreationEnd):
// on the page when the page is attached and not listed yet, or, while the
// one engine page creation in flight (newPage, under startMu) has not
// returned, in that creation's watch, which its attach reads. The listing's
// hold reads the page's end (unlistedEnd): a close lists no tab and fails
// the creation (errClosedBeforeListing), a crash alone lists the crashed tab
// a listed page's crash leaves (crash.go). A report made after the listing
// finds the listed page, whose tab the report's own work ends.

// creationEnd is what an engine reported ending a page before its listing:
// nothing, its close, or its process's end for crash. A close outweighs a
// crash, as for a popup.
type creationEnd struct {
	closed bool
	crash  string
}

// with is e and then the report of a close (closed) or of a crash.
func (e creationEnd) with(closed bool, crash string) creationEnd {
	switch {
	case closed:
		e.closed = true
	case e.crash == "":
		e.crash = crashFailure(crash)
	}
	return e
}

// ended reports whether anything ended the page.
func (e creationEnd) ended() bool { return e.closed || e.crash != "" }

// errClosedBeforeListing fails a creation or a restore whose page its
// engine reported closed before the Manager listed it.
var errClosedBeforeListing = errors.New("browser: the page closed as it opened")

// listCrashedLocked lists p, not listed yet and its process ended, as the
// crashed tab a listed page's crash leaves: a suspended record carrying
// the failure. The caller holds m.mu and has taken p out of its scope's
// creating pages.
func (m *Manager) listCrashedLocked(p *managedPage, failure string) {
	rec := recordOf(p, p.cachedInfo())
	rec.failure = failure
	m.suspended[p.id] = rec
}

// endedBeforeListing finishes a creation whose page ended before its
// listing (createReserved): the engine page is closed, the profile left
// empty disposed, and the tab, crashed or gone, published. A user's tab
// that crashed answers as one shown with its failure.
func (m *Manager) endedBeforeListing(p *managedPage, ended creationEnd, released *jarScope, user bool) error {
	p.driver.Close()
	m.mu.Lock()
	// Never listed live, so a popup its engine page opened is not adopted
	// (awaitOpener).
	m.unlistedLocked().put(p.driver.Handle(), struct{}{})
	m.listingChangedLocked()
	m.mu.Unlock()
	m.disposeReleased(endedDownloads{}, released, p.owner.id, p.id)
	if ended.closed {
		m.repairActivePage(p.owner)
	} else {
		m.ensureActivePage(p.owner, p.id)
	}
	m.emitThreadState(p.owner.id)
	m.syncPanePresentation(p.owner.id)
	if ended.closed {
		return errClosedBeforeListing
	}
	if err := m.persistThreads([]ThreadID{p.owner.id}); err != nil {
		// The tab is in memory; only a restart before the next save loses it.
		log.Printf("browser: page %s crashed as it opened: save the thread's pages: %v", p.id, err)
	}
	crashed := errors.New("browser: " + ended.crash)
	if user {
		return &shownOnTab{err: crashed}
	}
	return crashed
}

// creationWatch is the one engine page creation in flight: the ends its
// engine reported meanwhile of handles the Manager could not place, one of
// which may be the page being created. It lives from before the engine
// call to the attach, and holds at most an end per page the engine has,
// since no other page is created while it lives.
type creationWatch struct {
	ends map[ids.PageHandle]creationEnd
}

// noteCreationEnd records an engine's report of the close (closed) or the
// crash of handle for a page not listed yet, in the engine's call. A
// listed page, a page closing, a popup (whose admission keeps its ends) and
// a page the Manager will not list are left to the report's work.
func (m *Manager) noteCreationEnd(handle ids.PageHandle, closed bool, crash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pageByHandleLocked(handle) != nil || m.closingPageByHandleLocked(handle) {
		return
	}
	if p := m.creatingPageByHandleLocked(handle); p != nil {
		if !m.adoptingLocked(p) {
			p.unlistedEnd = p.unlistedEnd.with(closed, crash)
		}
		return
	}
	watch := m.creationWatch
	if watch == nil {
		return
	}
	if _, unlisted := m.unlistedLocked().get(handle); unlisted {
		return
	}
	if _, popup := m.admissions.Disposition(handle); popup {
		return
	}
	if watch.ends == nil {
		watch.ends = make(map[ids.PageHandle]creationEnd)
	}
	watch.ends[handle] = watch.ends[handle].with(closed, crash)
}

// closingPageByHandleLocked reports whether a page closing in any scope
// has handle. The caller holds m.mu.
func (m *Manager) closingPageByHandleLocked(handle ids.PageHandle) bool {
	for _, scope := range m.scopes {
		for _, c := range scope.closing {
			if c.p.driver != nil && c.p.driver.Handle() == handle {
				return true
			}
		}
	}
	return false
}
