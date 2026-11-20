package browser

import (
	"context"
	"fmt"
	"log"

	"agent-overflow/internal/browser/popupadmit"
)

// A popup's way from its announcement to its listing: the hold that
// reserves its page (reservePopup), the setup steps that attach and ready
// it (readyPopup), and the one commit that decides it (commitPopup). Every
// step's failure is an error readyPopup answers, and runAdoption hands it,
// with whatever driver it has, to commitPopup, so a failure anywhere in
// setup takes the popup's admission's verdict there and nowhere else.

// popupReservation is a popup's page from the hold that admits it to its
// commit: its jar, its opener's tab, and the page it reserves in the jar
// (scope.creating) and counts against every cap.
type popupReservation struct {
	popup  enginePopup
	scope  *jarScope
	opener *managedPage
	page   *managedPage
}

// runAdoption takes ownership of a page the engine opened by itself. The
// opener decides the thread, the workspace authority and the jar; this
// manager's own limits decide whether it survives, and the popup's
// admission whether it closed or crashed meanwhile.
//
// A popup opened by a popup still being adopted waits for that adoption
// (awaitOpener), and what the popup downloads before its fate is listed at
// its end (finishAdoption). From the hold that admits it to its commit,
// the popup's page reserves its jar's profile, as a page being created
// does (scope.creating), and counts against every cap, so a close of its
// opener cannot dispose the profile it is attached in and a burst of
// popups cannot pass the caps before any of them is listed.
func (m *Manager) runAdoption(popup enginePopup) {
	m.startAdoption(popup.Handle)
	var listed *managedPage
	defer func() { m.finishAdoption(popup.Handle, popup.Opener, listed) }()
	reservation, ok := m.reservePopup(popup)
	if !ok {
		// No page was reserved: the engine's page is closed, and the
		// adoption's end ends its admission.
		m.discardPopup(popup.Handle)
		return
	}
	driver, err := m.readyPopup(reservation)
	listed = m.commitPopup(reservation, driver, err)
}

// reservePopup admits popup once its opener is listed, under the opener's
// thread, workspace authority and jar, and reserves its page, unless the
// opener is gone or a cap refuses it.
func (m *Manager) reservePopup(popup enginePopup) (popupReservation, bool) {
	if !m.awaitOpener(popup.Opener) {
		return popupReservation{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var scope *jarScope
	var opener *managedPage
	for _, candidate := range m.scopes {
		if candidate.profile.Handle() != popup.Profile {
			continue
		}
		scope, opener = candidate, pageWithHandle(candidate, popup.Opener)
		break
	}
	if opener == nil || m.popupRefusedLocked(scope, opener) {
		return popupReservation{}, false
	}
	p := newManagedPage(opener.owner, opener.access)
	p.jarGen, p.authority = opener.jarGen, opener.authority
	p.info = PageInfo{ID: string(p.id), URL: truncateUTF8(popup.URL, maxBrowserURLBytes), Title: truncateUTF8(popup.Title, maxBrowserTitleBytes)}
	scope.creating[p] = struct{}{}
	m.adopting[popup.Handle] = p
	return popupReservation{popup: popup, scope: scope, opener: opener, page: p}, true
}

// readyPopup is the popup's setup between its reservation and its commit:
// its attach, the report held for it, its viewport, and its history. It
// answers the driver once attached, and the error of the step that failed,
// for commitPopup, which alone decides the popup.
func (m *Manager) readyPopup(r popupReservation) (pageDriver, error) {
	p, handle := r.page, r.popup.Handle
	p.touch()
	driver, err := r.scope.profile.AttachPage(context.Background(), handle, m.pageHooks(p))
	if err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	// Under m.mu: the jar's policy resolves a request to the reserved page
	// (jarAllows) while it loads, before it is listed.
	m.mu.Lock()
	p.attach(driver)
	held, hasHeld := m.takeHeldInfoLocked(handle)
	m.mu.Unlock()
	m.applyHeldInfo(p, held, hasHeld)
	if err := m.applyViewport(p); err != nil {
		return driver, fmt.Errorf("size: %w", err)
	}
	m.pageAttached(p)
	// The popup may have navigated before its listing, which reads no
	// history of its own: its Back and Forward go in the listing's push.
	m.readAdoptedHistory(p)
	return driver, nil
}

// commitPopup is the one decision on a reserved popup, whose setup ended
// with driver (nil when its attach failed) and setupErr: it lists the
// popup live, lists it crashed, or refuses it, and answers the live tab,
// or nil. The tab and the downloads the popup started are listed in one
// m.parkMu hold, which a page's close takes too (removeClosedPage), so a
// close never takes the tab before its downloads are in it. The listing
// waits out a jar switch of the opener's lifetime (lockStructure), whose
// commit closes the opener; without a cancel, since a switch ends with its
// one write.
//
// The opener may have closed meanwhile, with its thread's other pages
// (popupListableLocked): the thread's close is one such close, which
// closes every live page of the lifetime it detached until none is left.
// The admission's commit, in the same hold, reads what ended the popup
// before it, and the decision a created or restored page takes decides it
// (unlistedOutcomeLocked): a close refuses it; a crash alone lists it
// crashed, never live first, whether its setup succeeded or failed, while
// its lifetime may have records; a setup that failed with no crash
// refuses it. A popup not listed live is closed (its driver) or discarded
// (no driver), and a reservation no listing took ends.
func (m *Manager) commitPopup(r popupReservation, driver pageDriver, setupErr error) *managedPage {
	p, handle := r.page, r.popup.Handle
	if setupErr != nil {
		log.Printf("browser: adopt popup %s: %v", handle, setupErr)
	}
	m.parkMu.Lock()
	_ = m.lockStructure(context.Background(), r.opener.owner)
	listable := m.popupListableLocked(r.scope, r.popup)
	var verdict popupadmit.Verdict
	if listable {
		verdict = m.admissions.Commit(handle, setupErr != nil)
	}
	var listed *managedPage
	listedCrashed, reserved := false, true
	var released *jarScope
	switch m.unlistedOutcomeLocked(p, listable, r.scope, verdict) {
	case listLive:
		delete(r.scope.creating, p)
		m.adopting[handle] = nil
		reserved = false
		m.listPageLocked(r.scope, p)
		listed = p
		// A report made while the popup was adopted joins the listing,
		// whose push carries it.
		m.commitHeldInfoLocked(p, handle)
	case listCrashed:
		released = m.listCrashedPopupLocked(r.scope, p, handle, verdict.CrashReason())
		listedCrashed, reserved = true, false
	case refuseClosed, refuseFailed, refuseGone, retryStale:
		// Not listed: closed or discarded below, its reservation ended.
	}
	m.mu.Unlock()
	if listed == nil {
		m.parkMu.Unlock()
		if driver != nil {
			driver.Close()
		} else {
			m.discardPopup(handle)
		}
		if listedCrashed {
			m.crashedPopupListed(p, released)
		}
		if reserved {
			m.endPopupReservation(r.scope, p, handle)
		}
		return nil
	}
	if listing, ok := driver.(listingDriver); ok {
		listing.Listed()
	}
	if m.popupListing != nil {
		m.popupListing()
	}
	// Its downloads, before anything the engine held for the listing can
	// close it.
	m.resolveParkedLocked(handle, p, r.popup.Opener)
	m.parkMu.Unlock()
	// A new tab.
	m.pageAdded(p)
	if m.pageAdopted != nil {
		m.pageAdopted()
	}
	return listed
}

// popupListableLocked answers whether a popup may still be listed in
// scope: its jar is still open and its opener still a tab of it. A jar
// switch moves every live page of the thread to closing in the hold that
// advances the generation, so a live opener is always in its thread's
// current jar and the popup is too. The caller holds m.mu.
func (m *Manager) popupListableLocked(scope *jarScope, popup enginePopup) bool {
	return m.scopes[scope.jar.key] == scope && pageWithHandle(scope, popup.Opener) != nil
}
