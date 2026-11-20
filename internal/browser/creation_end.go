package browser

import (
	"errors"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/popupadmit"
)

// A page the Manager creates or restores can close or crash before it is
// listed, and its engine reports that by the page's handle, which the
// Manager learns only when the engine's NewPage returns. A popup has its
// admission for this (popupadmit); a created page has its creation end.
// The engine's report records the end in its own call (noteCreationEnd):
// on the page when the page is attached and not listed yet, or, while the
// one engine page creation in flight (newPage, under startMu) has not
// returned, in that creation's watch, which its attach reads. A report made
// after the listing finds the listed page, whose tab the report's own work
// ends.
//
// Every return path of a page's setup, its success and each step's
// failure, reaches the one decision on it (unlistedOutcomeLocked), in the
// hold that lists it: a created page's (createReserved), a restored one's
// (restorePage) and an adopted popup's (commitPopup). A page whose setup
// failed after its attach stays reserved in its jar until that decision,
// so the end its engine recorded is read whether its setup succeeded or not.

// errClosedBeforeListing fails a creation or a restore whose page its
// engine reported closed before the Manager listed it.
var errClosedBeforeListing = errors.New("browser: the page closed as it opened")

// listingOutcome is the decision on a page created, restored or adopted
// before it is listed (unlistedOutcomeLocked).
type listingOutcome int

const (
	// listLive: list the page as a live tab.
	listLive listingOutcome = iota + 1
	// listCrashed: its process ended first: list the crashed tab a listed
	// page's crash leaves (crash.go), never a live one.
	listCrashed
	// refuseClosed: its engine closed it first: list nothing and fail with
	// errClosedBeforeListing.
	refuseClosed
	// refuseFailed: a step of its setup failed with no end to show: list
	// nothing and fail with the step's error.
	refuseFailed
	// refuseGone: its tab is no longer the creation's (closed, its
	// lifetime closed, the browser closing), or it crashed while no page of
	// its lifetime may become a record (recordsAdmittedLocked): list
	// nothing.
	refuseGone
	// retryStale: set up in a jar its thread switched away from: create it
	// again in the thread's jar.
	retryStale
)

// unlistedOutcomeLocked is the one decision on p, created, restored or
// adopted and not listed yet, in the hold that lists it. current is
// whether its tab is still the creation's, and verdict its end's
// (popupadmit.Ending.Verdict with its setup's failure: the page's own
// unlistedEnd, or a popup's admission's Commit), read in this hold. A
// crash lists the crashed tab only while a page of its lifetime may
// become a record, and a page set up is listed live only in its thread's
// current jar. The caller holds m.mu and acts on the outcome in the same
// hold.
func (m *Manager) unlistedOutcomeLocked(p *managedPage, current bool, verdict popupadmit.Verdict) listingOutcome {
	if !current {
		return refuseGone
	}
	switch verdict.Kind() {
	case popupadmit.ListCrashed:
		if !m.recordsAdmittedLocked(p.owner) {
			return refuseGone
		}
		return listCrashed
	case popupadmit.Refuse:
		if verdict.Closed() {
			return refuseClosed
		}
		return refuseFailed
	}
	if !m.jarCurrentLocked(p) {
		return retryStale
	}
	return listLive
}

// recordsAdmittedLocked reports whether a page of tb may become a record
// (suspended or crashed): not while the Manager closes, whose Close saves
// the pages it has, or forgets every page (forgettingLocked), whose sweep
// of the records has run or is about to, and not for a lifetime its close
// detached, whose records that close dropped. The caller holds m.mu.
func (m *Manager) recordsAdmittedLocked(tb *threadBrowser) bool {
	return !m.closed && !m.forgettingLocked() && !tb.detached.Load()
}

// listCrashedLocked lists p, not listed yet and its process ended, as the
// crashed tab a listed page's crash leaves: a suspended record carrying
// failure. The caller holds m.mu and has taken p out of the creating
// pages it listed, and closes p's engine page after the hold.
func (m *Manager) listCrashedLocked(p *managedPage, failure string) {
	rec := recordOf(p, p.cachedInfo())
	rec.failure = failure
	m.suspended[p.id] = rec
}

// releaseUnlisted closes the engine page of p, set up in scope and not
// listed live, and ends its reservation there (abandonPage); nothing for
// a page whose setup ended before its attach (nil scope).
func (m *Manager) releaseUnlisted(scope *jarScope, p *managedPage) error {
	if scope == nil {
		return nil
	}
	return m.abandonPage(scope, p)
}

// creationWatch is the one engine page creation in flight: the ends its
// engine reported meanwhile of handles the Manager could not place, one of
// which may be the page being created. It lives from before the engine
// call to the attach, and holds at most an end per page the engine has,
// since no other page is created while it lives.
type creationWatch struct {
	ends map[ids.PageHandle]popupadmit.Ending
}

// noteCreationEnd records an engine's report of the close (closed) or the
// crash of handle for a page not listed yet, in the engine's call. A
// listed page, a page closing, a popup (whose admission keeps its ends) and
// a page the Manager will not list are left to the report's work.
func (m *Manager) noteCreationEnd(handle ids.PageHandle, closed bool, crash string) {
	end := popupadmit.Crashed(crash)
	if closed {
		end = popupadmit.Closed()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pageByHandleLocked(handle) != nil || m.closingPageByHandleLocked(handle) {
		return
	}
	if p := m.creatingPageByHandleLocked(handle); p != nil {
		if !m.adoptingLocked(p) {
			p.unlistedEnd = p.unlistedEnd.With(end)
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
		watch.ends = make(map[ids.PageHandle]popupadmit.Ending)
	}
	watch.ends[handle] = watch.ends[handle].With(end)
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
