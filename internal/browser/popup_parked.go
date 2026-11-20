package browser

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/quota"
)

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
// download the popup starts after it goes the same way at once
// (admitLate). Where a download goes is decided, and that page's download
// log taken, in one m.mu hold (holdParkedOwner), the hold every page's
// departure ends its log in, so an opener leaving at any point of that
// listing finds the row in its log or has already left as the popup's
// carrier. The tab
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
// its file over (listParked), and its refusal discards both, the file first
// (downloads_discard.go). One whose progress passes the per-download cap
// is stopped then. A parked download keeps the jar it is written in, whose
// directory holds its file until it is listed or removed, whether or not
// the jar is still open.
//
// Its engine settles a running one as it would any: on WebView2 the
// launcher cancels it with the popup's page and the engine ends it
// (hosted_popups.go); headless Chromium and WebKit let it finish.
//
// A popup never listed can end (its close received, or the Manager's
// refusal) while its nearest listed opener is live and that opener leave
// before the popup's downloads settle: before its engine processed the
// close, or after its engine page retired and before its fate runs. The
// Manager records the end, with the popup's openers (endedPopupLocked),
// and holds it until its engine has reported all it will of the popup
// (forgetEndedPopup), so the opener's leave carries the popup's
// downloads, as it would had the popup's fate come first: those the
// engine has not had admitted it lists itself (receivingDriver), and
// those parked here or reported after the fate are listed in the left
// page's log, as its leave would have (popupCarrier).

// popupCarrier is the leave of the page that carries a popup's downloads:
// the page, and why it left.
type popupCarrier struct {
	page   *managedPage
	reason string
}

// carriersLocked is the record of popup carriers, the newest orderSlack.
// The caller holds m.mu.
func (m *Manager) carriersLocked() *boundedMap[ids.PageHandle, popupCarrier] {
	if m.carriers == nil {
		m.carriers = newBoundedMap[ids.PageHandle, popupCarrier](orderSlack)
	}
	return m.carriers
}

// endedPopupsLocked is the record of popups never listed whose end the
// Manager knows and whose parked downloads have not settled, with the
// openers each was reported with, the newest orderSlack. The caller holds
// m.mu.
func (m *Manager) endedPopupsLocked() *boundedMap[ids.PageHandle, []ids.PageHandle] {
	if m.endedPopups == nil {
		m.endedPopups = newBoundedMap[ids.PageHandle, []ids.PageHandle](orderSlack)
	}
	return m.endedPopups
}

// endedPopupLocked records the end of popup handle, never listed, opened
// by openers, nearest first, until its parked downloads settle: a leave of
// its nearest listed opener meanwhile carries them
// (carryEndedPopupsLocked). A popup carried already keeps its carrier. The
// caller holds m.mu.
func (m *Manager) endedPopupLocked(handle ids.PageHandle, openers []ids.PageHandle) {
	if _, carried := m.carriersLocked().get(handle); carried {
		return
	}
	known, _ := m.endedPopupsLocked().get(handle)
	m.endedPopupsLocked().put(handle, mergeOpeners(slices.Clone(known), openers))
}

// carryEndedPopupsLocked makes p's leave for reason the carrier of each
// ended popup whose nearest listed opener (popupLineage) is p, and
// answers those popups. The caller holds m.mu, with p out of its tabs.
func (m *Manager) carryEndedPopupsLocked(p *managedPage, reason string) []ids.PageHandle {
	if m.endedPopups == nil {
		return nil
	}
	leaving := p.driver.Handle()
	var carried []ids.PageHandle
	for handle, openers := range m.endedPopups.all() {
		for _, opener := range m.popupLineage(handle, openers) {
			if opener == leaving {
				carried = append(carried, handle)
				break
			}
			if m.pageByHandleLocked(opener) != nil {
				break
			}
		}
	}
	for _, handle := range carried {
		m.endedPopups.delete(handle)
		m.carriersLocked().put(handle, popupCarrier{page: p, reason: reason})
	}
	return carried
}

// forgetEndedPopup forgets the end of popup handle (endedPopupLocked) and
// its carrier once its engine reports nothing more of it: its close was
// processed (removeClosedPage), or its discard returned (finishAdoption).
func (m *Manager) forgetEndedPopup(handle ids.PageHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endedPopupsLocked().delete(handle)
	m.carriersLocked().delete(handle)
}

// parkedPopup holds the downloads a popup started before its fate.
type parkedPopup struct {
	openers   []ids.PageHandle
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
	// the artifact root's, from its start until its row takes it
	// (listParked) or it is refused.
	reservation *quota.Reservation
	// file is the file the engine writes it into, in scope's directory,
	// which its listing hands to its row (listParked).
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
	listed  ids.PageHandle
	openers []ids.PageHandle
}

// parkDownload keeps a download of a popup not listed yet, or, when the
// popup's fate is known, lists it at once where that fate puts it
// (admitLate). Nobody has refused a download whose owner is not known
// yet, which it answers downloadUnowned: the popup's fate names no page
// that takes it, or, for one it parks on an engine whose downloads are
// AO's, its jar is not open or no page of its opener chain gives it a
// workspace. It answers downloadRefused past the bounds and when the
// workspace has no room for it. A download it does not admit leaves
// nothing parked.
func (m *Manager) parkDownload(event downloadStart) downloadAdmission {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
	if fate, known := m.fates().get(event.Popup); known {
		fate.openers = mergeOpeners(fate.openers, event.Openers)
		return m.admitLate(event, fate)
	}
	popup := m.parked[event.Popup]
	var openers []ids.PageHandle
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
	authority := m.openerAuthorityLocked(append([]ids.PageHandle{event.Popup}, m.popupLineage(event.Popup, openers)...))
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
func (m *Manager) parkPopupLocked(handle ids.PageHandle) *parkedPopup {
	if m.parked == nil {
		m.parked = make(map[ids.PageHandle]*parkedPopup)
		m.parkedIDs = make(map[downloadID]ids.PageHandle)
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
func (m *Manager) profileScopeLocked(handle profileHandle) *jarScope {
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
func (m *Manager) openerAuthorityLocked(handles []ids.PageHandle) *workspaceAuthority {
	for _, handle := range handles {
		if p := m.pageByHandleLocked(handle); p != nil {
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
// The downloads it started before are listed then: in p, in the leave
// that carries them (popupCarrier), or in the tab of its nearest listed
// opener. A download's start and newest progress are
// applied in m.parkMu, so a later progress waits for them and lands after.
func (m *Manager) resolveParked(handle ids.PageHandle, p *managedPage, openers ...ids.PageHandle) {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
	m.resolveParkedLocked(handle, p, openers...)
}

// resolveParkedLocked is resolveParked for a caller that holds m.parkMu.
func (m *Manager) resolveParkedLocked(handle ids.PageHandle, p *managedPage, openers ...ids.PageHandle) {
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
	for _, d := range popup.downloads {
		placed, discard, canceled := m.placeParked(handle, fate, d, true)
		switch {
		case !placed:
			log.Printf("browser: download %s of popup %s is listed in no tab: no page that opened the popup is open", d.start.ID, handle)
			m.refuseParked(d)
		case canceled:
			m.stopRefused(d, discard)
		}
	}
}

// placeParked lists d, a parked download of popup handle whose fate is
// fate, in the page that fate gives it, chosen with its log held so no
// page's departure comes between the choice and the row
// (holdParkedOwner): the popup's tab while it is listed; else, for a
// popup never listed, the leave that carries it (popupCarrier); else,
// with live, its nearest listed opener (popupLineage). discard is the
// discard of a row a carrier's leave settled canceled (listParked), which
// the caller finishes once its engine stops writing the file. It answers
// placed false, with d untouched, when no page takes it. The caller holds
// m.parkMu.
func (m *Manager) placeParked(handle ids.PageHandle, fate popupFate, d *parkedDownload, live bool) (placed bool, discard downloadDiscard, canceled bool) {
	owner, ok := m.holdParkedOwner(handle, fate, live, d.start)
	if !ok {
		return false, downloadDiscard{}, false
	}
	discard, canceled = m.listParked(owner, d)
	return true, discard, canceled
}

// parkedOwner is the page a download of a popup with a fate is listed in,
// with its jar while it is listed, its download log held, the download's
// source as the page was chosen (downloadSource), and, for a carrier's
// page, the carrier.
type parkedOwner struct {
	page    *managedPage
	scope   *jarScope
	held    *downloadLogHold
	source  string
	carrier popupCarrier
	carried bool
}

// holdParkedOwner chooses the page download start of popup handle, whose
// fate is fate, is listed in (parkedOwnerLocked), reads the download's
// source there, and takes that page's download log, all in one m.mu hold,
// the hold every page's departure ends its log in: a page chosen while
// listed stays admitting until the log is let go, and a navigation while
// the log is waited for does not change the source. It answers false when
// no page takes the download.
func (m *Manager) holdParkedOwner(handle ids.PageHandle, fate popupFate, live bool, start downloadStart) (parkedOwner, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, scope, carrier, carried := m.parkedOwnerLocked(handle, fate, live)
	if p == nil {
		return parkedOwner{}, false
	}
	source := downloadSource(p, start)
	if m.downloadAdmitting != nil {
		m.downloadAdmitting()
	}
	return parkedOwner{page: p, scope: scope, held: p.downloads.hold(), source: source, carrier: carrier, carried: carried}, true
}

// listParked adds d's row to owner's log, which it lets go: the row takes
// d's file and reservation, and d's newest progress is applied before the
// log is let go. A row in a carrier's log then settles as that leave
// settled its others (departLocked), and discard is the discard of a row
// that settled canceled.
func (m *Manager) listParked(owner parkedOwner, d *parkedDownload) (discard downloadDiscard, canceled bool) {
	p, held := owner.page, owner.held
	entry := m.downloadEntry(d.start)
	// Its bytes stay held, against the workspace they were taken from.
	entry.reservation = quota.Take(&d.reservation)
	entry.source, d.file = d.file, downloadFile{}
	listed := m.noteDownloadStartLocked(p, held.add(entry), owner.source)
	if d.newest != nil {
		m.applyHeldRow(p, owner.scope, held, *d.newest)
	} else {
		held.release()
	}
	m.publishDownloads(listed)
	return m.settleCarried(owner, d.start.ID)
}

// settleCarried settles the row of download id that was just listed in
// owner's log as owner's leave settled its others, when owner is a
// carrier (departLocked): a row whose end its engine received waits for
// it, and any other is canceled, which discard is the discard of. A row
// in a listed page is left as it is.
func (m *Manager) settleCarried(owner parkedOwner, id downloadID) (discard downloadDiscard, canceled bool) {
	if !owner.carried {
		return downloadDiscard{}, false
	}
	p := owner.page
	m.mu.Lock()
	held := p.downloads.hold()
	var note downloadNote
	if row := held.entry(id); row != nil && !row.phase.ended() {
		note, discard, canceled = m.departLocked(p, row, owner.carrier.reason)
	}
	held.release()
	m.mu.Unlock()
	if canceled {
		m.publishDownloads(note)
	}
	return discard, canceled
}

// admitLate admits a download popup event.Popup starts after its fate,
// fate: at once, in the page that fate puts it in, chosen with its log
// held (holdParkedOwner), as any download starting in that page is
// admitted (listStart), and it answers downloadUnowned when no page takes
// it. In a carrier's log the row then settles as that leave settled its
// others (settleCarried). An engine whose downloads are AO's keeps a file
// only for a listed page, in the download's open jar and against that
// page's workspace: for a carrier it answers downloadRefused with a row
// canceled as the leave canceled its others, and with the jar closed
// downloadUnowned, and either way the engine ends the download and
// removes its file.
func (m *Manager) admitLate(event downloadStart, fate popupFate) downloadAdmission {
	m.mu.Lock()
	scope := m.profileScopeLocked(event.Profile)
	m.mu.Unlock()
	owner, ok := m.holdParkedOwner(event.Popup, fate, true, event)
	if !ok {
		return downloadUnowned
	}
	if m.downloadsToArtifacts() {
		switch {
		case owner.carried:
			note := m.listRefused(owner.page, owner.held, event, owner.carrier.reason, owner.source)
			owner.held.release()
			m.publishDownloads(note)
			return downloadRefused
		case scope == nil:
			owner.held.release()
			return downloadUnowned
		}
	}
	note, admitted := m.listStart(owner.page, owner.held, event, m.downloadFileOf(scope, event.ID), owner.source)
	owner.held.release()
	m.publishDownloads(note)
	if !admitted {
		return downloadRefused
	}
	if discard, canceled := m.settleCarried(owner, event.ID); canceled {
		m.stopRefused(&parkedDownload{start: event, scope: scope}, discard)
	}
	return downloadAdmitted
}

// parkedOwnerLocked answers the page a parked download of popup handle,
// whose fate is fate, is listed in (placeParked), with its jar while it
// is listed, and, for a carrier's page, the carrier. The caller holds
// m.mu.
func (m *Manager) parkedOwnerLocked(handle ids.PageHandle, fate popupFate, live bool) (*managedPage, *jarScope, popupCarrier, bool) {
	if fate.listed == "" {
		if carrier, carried := m.carriersLocked().get(handle); carried {
			return carrier.page, nil, carrier, true
		}
	}
	if !live {
		return nil, nil, popupCarrier{}, false
	}
	if fate.listed != "" {
		if p, scope := m.listedPageLocked(fate.listed); p != nil {
			return p, scope, popupCarrier{}, false
		}
	}
	for _, opener := range m.popupLineage(handle, fate.openers) {
		if p, scope := m.listedPageLocked(opener); p != nil {
			return p, scope, popupCarrier{}, false
		}
	}
	return nil, nil, popupCarrier{}, false
}

// listedPageLocked answers the listed page whose driver has handle, with
// its jar. The caller holds m.mu.
func (m *Manager) listedPageLocked(handle ids.PageHandle) (*managedPage, *jarScope) {
	for _, scope := range m.scopes {
		if p := pageWithHandle(scope, handle); p != nil {
			return p, scope
		}
	}
	return nil, nil
}

// refuseParked ends a parked download no tab took: it stops, once
// (stopParked), and is discarded, its file removed before its reservation
// settles. The caller holds m.parkMu.
func (m *Manager) refuseParked(d *parkedDownload) {
	m.stopRefused(d, d.discard())
}

// stopRefused stops parked download d once (stopParked) and finishes
// discard, what of its file and reservation AO ended. The caller holds
// m.parkMu.
func (m *Manager) stopRefused(d *parkedDownload, discard downloadDiscard) {
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

// fates is the record of popup fates, the newest orderSlack. The caller
// holds m.parkMu.
func (m *Manager) fates() *boundedMap[ids.PageHandle, popupFate] {
	if m.popupFates == nil {
		m.popupFates = newBoundedMap[ids.PageHandle, popupFate](orderSlack)
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
		fate, _ := m.fates().get(handle)
		for _, d := range popup.downloads {
			placed, discard, canceled := m.placeParked(handle, fate, d, false)
			switch {
			case !placed:
				log.Printf("browser: download %s of popup %s is listed in no tab: the browser closed", d.start.ID, handle)
				discards = append(discards, d.discard())
			case canceled:
				discards = append(discards, discard)
			}
		}
	}
	m.parked, m.parkedIDs, m.popupFates = nil, nil, nil
	m.mu.Lock()
	m.carriers, m.endedPopups = nil, nil
	m.mu.Unlock()
	return discards
}

// popupLineage answers the pages that opened popup handle, nearest first,
// each once: named, the openers its engine named, then the whole chain
// its admission and its ancestors' keep (popupadmit.Registry.Ancestors),
// which is whole while the popup or a popup of it is live, so a popup of
// a popup that went still finds the tab that opened its parent however
// many handoffs ago. The registry bounds that chain by what is live;
// maxPopupOpeners bounds only the openers an engine names. A popup whose
// admission is no longer known has only named.
func (m *Manager) popupLineage(handle ids.PageHandle, named []ids.PageHandle) []ids.PageHandle {
	lineage := slices.Clone(named)
	for _, ancestor := range m.admissions.Ancestors(handle).Chain() {
		if !slices.Contains(lineage, ancestor) {
			lineage = append(lineage, ancestor)
		}
	}
	return lineage
}

// mergeOpeners appends to openers those of more it lacks, in order,
// within maxPopupOpeners.
func mergeOpeners(openers, more []ids.PageHandle) []ids.PageHandle {
	for _, opener := range more {
		if opener != "" && !slices.Contains(openers, opener) && len(openers) < maxPopupOpeners {
			openers = append(openers, opener)
		}
	}
	return openers
}
