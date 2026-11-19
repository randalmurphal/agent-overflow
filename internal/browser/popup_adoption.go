package browser

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/browser/quota"
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
// A download a popup starts before its tab is listed names the popup by
// the handle the Manager adopts it under, and its openers, nearest first
// (downloadStart.Popup, Openers). The Manager keeps it, with the newest
// progress the engine reports for it (parkedPopup), until the popup's fate
// is known, and lists it then: in the popup's tab when the popup is
// adopted, and otherwise (the popup closed, was refused at a cap, or its
// attach failed) in the tab of its nearest listed opener, as the download
// it is: running, finished or ended. A popup's fate is known at the end of
// its adoption, at its page's close (removeClosedPage), or after
// popupParkTimeout if neither comes, and is remembered (popupFate), so a
// download the popup starts after it goes the same way at once. The tab
// and its downloads are listed in one m.parkMu hold, which a page's close
// also takes (removeClosedPage), so a close never takes the tab before the
// downloads, with their newest progress, are in it. A download whose
// workspace or a bound refuses it is refused, and one no listed page, open
// jar or workspace can own yet is unowned (parkDownload): either way its
// engine ends it (downloadAdmission), the file AO keeps for it (staged or
// an artifact) goes, and nothing is parked for it; WebView2's finished
// file stays in the Downloads folder, where it saved it itself.
// On an engine whose downloads are AO's, a parked download holds what a
// listed one does from its start: maxDownloadBytes of its workspace's
// allowance, the workspace of the nearest page that opened it, and of the
// artifact quota, or it is refused. Its listing takes that reservation and
// its file over (admitTo), and its refusal discards both, the file first
// (downloads_discard.go). One whose progress passes the per-download cap
// is stopped then. A parked download keeps the jar it is written in, whose
// directory holds its file until it is listed or removed, whether or not
// the jar is still open.
//
// Its engine settles a running one as it would any: on WebView2 the
// launcher cancels it with the popup's page and the engine ends it
// (hosted_popups.go); headless Chromium and WebKit let it finish.

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

// parkedPopup holds the downloads a popup started before its fate.
type parkedPopup struct {
	openers   []string
	downloads []*parkedDownload
	timer     *time.Timer
}

// parkedDownload is one download of a popup not listed yet. Guarded by
// m.parkMu.
type parkedDownload struct {
	start  downloadStart
	newest *downloadProgress
	// scope is the jar the download is written in, nil when none was open:
	// its cancel goes to the jar's profile while it is open, and its file is
	// in the jar's download directory either way.
	scope *jarScope
	// reservation is, on an engine whose downloads are AO's, the quota the
	// download holds, maxDownloadBytes of its workspace's allowance and of
	// the artifact root's, from its start until its row takes it (admitTo)
	// or it is refused.
	reservation *quota.Reservation
	// file is the file the engine writes it into, in scope's directory,
	// which its listing hands to its row (admitTo).
	file downloadFile
	// phase is whether AO has stopped it (stop).
	phase parkedPhase
}

// parkedPhase is a parked download's lifecycle until its fate, after which
// it is no longer parked: running, or stopped by AO (over maxDownloadBytes,
// or refused), after which it takes no further progress and its engine
// has been asked to cancel it once.
type parkedPhase int

const (
	parkedRunning parkedPhase = iota
	parkedStopped
)

// stop moves d to parkedStopped and answers whether it ran until now, for
// the one caller that then stops it in its engine (stopParked). The caller
// holds m.parkMu.
func (d *parkedDownload) stop() bool {
	switch d.phase {
	case parkedRunning:
		d.phase = parkedStopped
		return true
	case parkedStopped:
		return false
	}
	panic(fmt.Sprintf("browser: parked download in phase %d", d.phase))
}

// popupFate is what became of a popup: listed is the driver handle of its
// tab, empty when it never became one, and openers the pages that opened
// it, nearest first, whose nearest listed one takes its downloads then.
type popupFate struct {
	listed  string
	openers []string
}

// startAdoption records the adoption of handle, which its admission
// (announcePopup) runs once.
func (m *Manager) startAdoption(handle string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.adopting == nil {
		m.adopting = make(map[string]*managedPage)
	}
	m.adopting[handle] = nil
}

// finishAdoption ends the adoption of handle, opened by opener, listed as p
// or not at all, wakes the popups it opened (awaitOpener), and lists the
// downloads it started meanwhile. A popup not listed lets go of the report
// held for it, and none is held for it after (updatePageInfo).
func (m *Manager) finishAdoption(handle, opener string, p *managedPage) {
	m.mu.Lock()
	delete(m.adopting, handle)
	if p == nil {
		m.admissions.Discard(popupadmit.Handle(handle))
		m.unlistedLocked().put(handle, struct{}{})
		m.heldInfo.delete(handle)
	}
	m.listingChangedLocked()
	m.mu.Unlock()
	m.resolveParked(handle, p, opener)
}

// listPageLocked makes p a tab of scope, the one way a page joins the
// tabs (a create, a restore, an adoption), and wakes the popups waiting
// for it as their opener. The caller holds m.mu.
func (m *Manager) listPageLocked(scope *jarScope, p *managedPage) {
	scope.pages[p.id] = p
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
func (m *Manager) awaitOpener(opener string) bool {
	deadline := time.NewTimer(popupOpenerWait)
	defer deadline.Stop()
	for {
		m.mu.Lock()
		if m.listedPageLocked(opener) != nil {
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
func (m *Manager) unlistedLocked() *boundedMap[string, struct{}] {
	if m.unlisted == nil {
		m.unlisted = newBoundedMap[string, struct{}](orderSlack)
	}
	return m.unlisted
}

// listedPageLocked answers the listed page whose driver has handle, or
// nil. The caller holds m.mu.
func (m *Manager) listedPageLocked(handle string) *managedPage {
	for _, scope := range m.scopes {
		if p := pageWithHandle(scope, handle); p != nil {
			return p
		}
	}
	return nil
}

// parkDownload keeps a download of a popup not listed yet, or lists it at
// once when the popup's fate is known (admitTo answers then). Nobody has
// refused a download whose owner is not known yet, which it answers
// downloadUnowned: the popup's fate names no listed page, or, on an engine
// whose downloads are AO's, its jar is not open or no page of its opener
// chain gives it a workspace. It answers downloadRefused past the bounds
// and when the workspace has no room for it. A download it does not admit
// leaves nothing parked.
func (m *Manager) parkDownload(event downloadStart) downloadAdmission {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
	if fate, known := m.fates().get(event.Popup); known {
		fate.openers = mergeOpeners(fate.openers, event.Openers)
		target := m.fateTarget(event.Popup, fate)
		if target == nil {
			return downloadUnowned
		}
		m.mu.Lock()
		scope := m.profileScopeLocked(event.Profile)
		m.mu.Unlock()
		return m.admitTo(target, event, m.downloadFileOf(scope, event.ID), nil)
	}
	popup := m.parked[event.Popup]
	var openers []string
	switch {
	case popup == nil && len(m.parked) >= maxParkedPopups:
		return downloadRefused
	case popup != nil && len(popup.downloads) >= maxParkedDownloads:
		return downloadRefused
	case popup != nil:
		openers = popup.openers
	}
	openers = mergeOpeners(openers, event.Openers)
	m.mu.Lock()
	d := &parkedDownload{start: event, scope: m.profileScopeLocked(event.Profile)}
	authority := m.openerAuthorityLocked(append([]string{event.Popup}, openers...))
	m.mu.Unlock()
	d.file = m.downloadFileOf(d.scope, event.ID)
	if m.downloadsToArtifacts() {
		// Its workspace's allowance and the artifact quota, as a listed
		// download's, before the engine writes a byte of it.
		if d.scope == nil || authority == nil {
			return downloadUnowned
		}
		reservation, err := m.reserveDownload(authority)
		if err != nil {
			return downloadRefused
		}
		d.reservation = reservation
	}
	if popup == nil {
		popup = m.parkPopupLocked(event.Popup)
	}
	popup.openers = openers
	popup.downloads = append(popup.downloads, d)
	m.parkedIDs[event.ID] = event.Popup
	return downloadAdmitted
}

// parkPopupLocked starts holding downloads for popup handle until its fate,
// or popupParkTimeout. The caller holds m.parkMu.
func (m *Manager) parkPopupLocked(handle string) *parkedPopup {
	if m.parked == nil {
		m.parked = make(map[string]*parkedPopup)
		m.parkedIDs = make(map[string]string)
	}
	timeout := popupParkTimeout
	if m.parkTimeout > 0 {
		timeout = m.parkTimeout
	}
	popup := &parkedPopup{}
	popup.timer = time.AfterFunc(timeout, func() { m.resolveParked(handle, nil) })
	m.parked[handle] = popup
	return popup
}

// parkProgressLocked keeps the newest progress of a parked download, and
// answers whether id is one. One past maxDownloadBytes on an engine whose
// downloads are AO's is stopped then, keeping that progress, which lists
// it canceled over the cap at its fate. The caller holds m.parkMu.
func (m *Manager) parkProgressLocked(event downloadProgress) bool {
	handle, ok := m.parkedIDs[event.ID]
	if !ok {
		return false
	}
	for _, d := range m.parked[handle].downloads {
		if d.start.ID != event.ID || d.phase == parkedStopped {
			continue
		}
		newest := event
		d.newest = &newest
		if m.downloadsToArtifacts() && int64(event.Received) > maxDownloadBytes && !event.State.stopped() && d.stop() {
			// Its file goes now; its reservation stays for its row, which
			// lists it canceled over the cap, or its refusal.
			m.stopParked(d, downloadDiscard{id: d.start.ID, file: d.file})
		}
	}
	return true
}

// profileScopeLocked answers the open jar whose profile has handle, or nil.
// The caller holds m.mu.
func (m *Manager) profileScopeLocked(handle string) *jarScope {
	for _, scope := range m.scopes {
		if scope.profile.Handle() == handle {
			return scope
		}
	}
	return nil
}

// openerAuthorityLocked answers the workspace authority of the first of
// handles that is a tab or a popup being adopted, or nil. The caller holds
// m.mu.
func (m *Manager) openerAuthorityLocked(handles []string) *workspaceAuthority {
	for _, handle := range handles {
		if p := m.listedPageLocked(handle); p != nil {
			return p.authority
		}
		if p := m.adopting[handle]; p != nil {
			return p.authority
		}
	}
	return nil
}

// resolveParked records the fate of popup handle: listed as p, or, with p
// nil, not listed, opened by openers besides those its downloads named.
// The downloads it started before are listed then: in p, or in the tab of
// its nearest listed opener. A download's start and newest progress are
// applied in m.parkMu, so a later progress waits for them and lands after.
func (m *Manager) resolveParked(handle string, p *managedPage, openers ...string) {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
	m.resolveParkedLocked(handle, p, openers...)
}

// resolveParkedLocked is resolveParked for a caller that holds m.parkMu.
func (m *Manager) resolveParkedLocked(handle string, p *managedPage, openers ...string) {
	fate, _ := m.fates().get(handle)
	if p != nil {
		fate.listed = p.driver.Handle()
	}
	fate.openers = mergeOpeners(fate.openers, openers)
	popup := m.parked[handle]
	if popup != nil {
		delete(m.parked, handle)
		popup.timer.Stop()
		for _, d := range popup.downloads {
			delete(m.parkedIDs, d.start.ID)
		}
		fate.openers = mergeOpeners(fate.openers, popup.openers)
	}
	m.fates().put(handle, fate)
	if popup == nil {
		return
	}
	target := m.fateTarget(handle, fate)
	for _, d := range popup.downloads {
		if target == nil {
			log.Printf("browser: download %s of popup %s is listed in no tab: no page that opened the popup is open", d.start.ID, handle)
			m.refuseParked(d)
			continue
		}
		// Its listing takes its reservation over (admitTo), which only a
		// tab that left meanwhile refuses.
		if m.admitTo(target, d.start, d.file, d) != downloadAdmitted {
			log.Printf("browser: download %s of popup %s is listed in no tab: its tab closed as it was listed", d.start.ID, handle)
			m.refuseParked(d)
			continue
		}
		if d.newest != nil {
			m.applyDownloadProgress(*d.newest)
		}
	}
}

// refuseParked ends a parked download no tab took: it stops, once
// (stopParked), and is discarded, its file removed before its reservation
// settles. The caller holds m.parkMu.
func (m *Manager) refuseParked(d *parkedDownload) {
	discard := d.discard()
	if d.stop() {
		m.stopParked(d, discard)
		return
	}
	// Stopped before: its engine was asked to cancel it then.
	go m.finishDiscard(discard)
}

// stopParked has the engine cancel a parked download still running, and
// then finishes discard, its file and whatever of its reservation it took.
// The caller holds m.parkMu.
func (m *Manager) stopParked(d *parkedDownload, discard downloadDiscard) {
	scope := d.scope
	m.mu.Lock()
	open := scope != nil && m.scopes[scope.jar.key] == scope
	m.mu.Unlock()
	running := d.newest == nil || d.newest.State.running()
	// With its profile gone, so is the browser that ran it. Off the
	// caller's locks: a cancel may wait on the engine's UI thread.
	go func() {
		if running && open {
			if err := scope.profile.CancelDownload(d.start.ID); err != nil && !errors.Is(err, errDownloadEnded) {
				log.Printf("browser: cancel download %s listed in no tab: %v", d.start.ID, err)
			}
		}
		m.finishDiscard(discard)
	}()
}

// fateTarget answers the page that takes the downloads of popup handle:
// its tab while listed, else (never listed, or closed since by any path)
// its nearest listed opener, or nil. Its openers are those its engine
// named and, past them, the chain its admission and its ancestors' keep
// (popupadmit.Registry.Ancestors), so a popup of a popup that went still
// finds the tab that opened its parent.
func (m *Manager) fateTarget(handle string, fate popupFate) *managedPage {
	openers := fate.openers
	for _, ancestor := range m.admissions.Ancestors(popupadmit.Handle(handle)) {
		openers = mergeOpeners(openers, []string{string(ancestor)})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if fate.listed != "" {
		if p := m.listedPageLocked(fate.listed); p != nil {
			return p
		}
	}
	for _, opener := range openers {
		if p := m.listedPageLocked(opener); p != nil {
			return p
		}
	}
	return nil
}

// fates is the record of popup fates, the newest orderSlack. The caller
// holds m.parkMu.
func (m *Manager) fates() *boundedMap[string, popupFate] {
	if m.popupFates == nil {
		m.popupFates = newBoundedMap[string, popupFate](orderSlack)
	}
	return m.popupFates
}

// dropParked refuses every parked download and forgets every fate, for a
// closing browser. It answers their discards, for the caller to finish
// once the engine that writes their files has stopped.
func (m *Manager) dropParked() []downloadDiscard {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
	var discards []downloadDiscard
	for handle, popup := range m.parked {
		popup.timer.Stop()
		for _, d := range popup.downloads {
			log.Printf("browser: download %s of popup %s is listed in no tab: the browser closed", d.start.ID, handle)
			discards = append(discards, d.discard())
		}
	}
	m.parked, m.parkedIDs, m.popupFates = nil, nil, nil
	return discards
}

// mergeOpeners appends to openers those of more it lacks, in order,
// within maxPopupOpeners.
func mergeOpeners(openers, more []string) []string {
	for _, opener := range more {
		if opener != "" && !slices.Contains(openers, opener) && len(openers) < maxPopupOpeners {
			openers = append(openers, opener)
		}
	}
	return openers
}
