package browser

import (
	"time"

	"agent-overflow/internal/browser/ids"
)

// A popup between the engine's report of it (PopupOpened) and the
// Manager's decision on it, for every engine.
//
// Adoptions are recorded by handle while they run (adoptPopup). A popup
// opened by a popup still being adopted waits for that adoption: adopted,
// it proceeds under its listed opener; discarded, it is discarded too. An
// engine dispatches each popup report on a goroutine of its own, so a
// popup whose opener is neither listed nor being adopted yet waits, as
// long, for its opener to become either. The wait is bounded
// (popupOpenerWait), so a stuck opener holds no popup for good.
//
// The downloads a popup starts before its fate is known wait in
// popup_parked.go.

const (
	// popupOpenerWait bounds how long a popup waits for its opener's
	// adoption, or for its opener to be known at all.
	popupOpenerWait = 10 * time.Second
	// popupParkTimeout bounds how long the downloads of a popup whose fate
	// never comes wait to be listed.
	popupParkTimeout = 30 * time.Second
	// maxParkedPopups bounds the popups holding downloads, and
	// maxParkedDownloads the downloads each holds. A download past either
	// is refused as it starts, so its engine cancels it.
	maxParkedPopups    = maxPagesTotal
	maxParkedDownloads = 16
	// maxPopupOpeners bounds the openers kept for one popup.
	maxPopupOpeners = maxPagesTotal
)

// startAdoption records the adoption of handle, which its admission
// (announcePopup) runs once.
func (m *Manager) startAdoption(handle ids.PageHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.adopting == nil {
		m.adopting = make(map[ids.PageHandle]*managedPage)
	}
	m.adopting[handle] = nil
}

// finishAdoption ends the adoption of handle, opened by opener, listed as p
// or not at all, lists the downloads it started meanwhile, and wakes the
// popups it opened (awaitOpener). The downloads are listed while its
// admission still keeps its chain live. A popup not listed lets go of the
// report held for it, and none is held for it after (updatePageInfo); one
// its engine retired (retired, discardPopup) has its end forgotten
// (forgetEndedPopup), and any other's stays until its close is processed
// (removeClosedPage) or the record's bound drops it.
func (m *Manager) finishAdoption(handle, opener ids.PageHandle, p *managedPage, retired bool) {
	m.resolveParked(handle, p, opener)
	if retired {
		m.forgetEndedPopup(handle)
	}
	m.mu.Lock()
	delete(m.adopting, handle)
	if p == nil {
		m.admissions.Discard(handle)
		m.unlistedLocked().put(handle, struct{}{})
		m.heldInfo.delete(handle)
	}
	m.listingChangedLocked()
	m.mu.Unlock()
}

// listPageLocked makes p a tab of scope, the one way a page joins the
// tabs (a create, a restore, an adoption), selects it when its thread has
// no active tab, and wakes the popups waiting for it as their opener. The
// caller holds m.mu.
func (m *Manager) listPageLocked(scope *jarScope, p *managedPage) {
	scope.pages[p.id] = p
	m.ensureActivePageLocked(p.owner, p.id)
	m.listingChangedLocked()
}

// listingChangedLocked wakes the popups waiting for an opener
// (awaitOpener): a page joined the tabs (listPageLocked), a page created
// or restored will not (abandonPage), or an adoption ended. A listed
// page's leaving wakes no one, since nothing waits for a listed opener.
// The caller holds m.mu.
func (m *Manager) listingChangedLocked() {
	if m.listingChanged != nil {
		close(m.listingChanged)
		m.listingChanged = nil
	}
}

// awaitOpener waits until opener, a page handle, is listed, or will not be
// (unlistedLocked), within popupOpenerWait, and answers whether it is
// listed. An opener still being adopted, or not reported yet (an engine
// reports each popup on a goroutine of its own), is waited for.
func (m *Manager) awaitOpener(opener ids.PageHandle) bool {
	deadline := time.NewTimer(popupOpenerWait)
	defer deadline.Stop()
	for {
		m.mu.Lock()
		if m.pageByHandleLocked(opener) != nil {
			m.mu.Unlock()
			return true
		}
		if _, gone := m.unlistedLocked().get(opener); gone {
			m.mu.Unlock()
			return false
		}
		if m.listingChanged == nil {
			m.listingChanged = make(chan struct{})
		}
		changed := m.listingChanged
		m.mu.Unlock()
		select {
		case <-changed:
		case <-deadline.C:
			return false
		}
	}
}

// unlistedLocked is the newest handles that will not be listed again:
// popups whose adoption ended without a listing, and pages that left. The
// caller holds m.mu.
func (m *Manager) unlistedLocked() *boundedMap[ids.PageHandle, struct{}] {
	if m.unlisted == nil {
		m.unlisted = newBoundedMap[ids.PageHandle, struct{}](orderSlack)
	}
	return m.unlisted
}
