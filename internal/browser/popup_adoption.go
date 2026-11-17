package browser

import (
	"errors"
	"log"
	"slices"
	"time"
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
// download the popup starts after it goes the same way at once. With no
// listed page left to take it, the download is refused, and its engine
// cancels it, or, if it finished, its file stays where the engine saved it.
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

type parkedDownload struct {
	start  downloadStart
	newest *downloadProgress
}

// popupFate is what became of a popup: listed is the driver handle of its
// tab, empty when it never became one, and openers the pages that opened
// it, nearest first, whose nearest listed one takes its downloads then.
type popupFate struct {
	listed  string
	openers []string
}

// startAdoption records the adoption of handle, or answers false when one
// already runs.
func (m *Manager) startAdoption(handle string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.adopting == nil {
		m.adopting = make(map[string]struct{})
	}
	if _, running := m.adopting[handle]; running {
		return false
	}
	m.adopting[handle] = struct{}{}
	return true
}

// finishAdoption ends the adoption of handle, opened by opener, listed as p
// or not at all, wakes the popups it opened (awaitOpener), and lists the
// downloads it started meanwhile.
func (m *Manager) finishAdoption(handle, opener string, p *managedPage) {
	m.mu.Lock()
	delete(m.adopting, handle)
	if p == nil {
		m.unlistedLocked().put(handle, struct{}{})
	}
	if m.adoptionsChanged != nil {
		close(m.adoptionsChanged)
		m.adoptionsChanged = nil
	}
	m.mu.Unlock()
	m.resolveParked(handle, p, opener)
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
		if m.adoptionsChanged == nil {
			m.adoptionsChanged = make(chan struct{})
		}
		changed := m.adoptionsChanged
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
// once when the popup's fate is known, and answers false past the bounds
// or when no listed page takes it.
func (m *Manager) parkDownload(event downloadStart) bool {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
	if fate, known := m.fates().get(event.Popup); known {
		fate.openers = mergeOpeners(fate.openers, event.Openers)
		target := m.fateTarget(fate)
		return target != nil && m.admitTo(target, event)
	}
	if m.parked == nil {
		m.parked = make(map[string]*parkedPopup)
		m.parkedIDs = make(map[string]string)
	}
	popup := m.parked[event.Popup]
	if popup == nil {
		if len(m.parked) >= maxParkedPopups {
			return false
		}
		popup = &parkedPopup{}
		handle := event.Popup
		timeout := popupParkTimeout
		if m.parkTimeout > 0 {
			timeout = m.parkTimeout
		}
		popup.timer = time.AfterFunc(timeout, func() { m.resolveParked(handle, nil) })
		m.parked[handle] = popup
	}
	if len(popup.downloads) >= maxParkedDownloads {
		return false
	}
	popup.openers = mergeOpeners(popup.openers, event.Openers)
	popup.downloads = append(popup.downloads, &parkedDownload{start: event})
	m.parkedIDs[event.ID] = event.Popup
	return true
}

// parkProgressLocked keeps the newest progress of a parked download, and
// answers whether id is one. The caller holds m.parkMu.
func (m *Manager) parkProgressLocked(event downloadProgress) bool {
	handle, ok := m.parkedIDs[event.ID]
	if !ok {
		return false
	}
	for _, d := range m.parked[handle].downloads {
		if d.start.ID == event.ID {
			newest := event
			d.newest = &newest
		}
	}
	return true
}

// resolveParked records the fate of popup handle: listed as p, or, with p
// nil, not listed, opened by openers besides those its downloads named.
// The downloads it started before are listed then: in p, or in the tab of
// its nearest listed opener. A download's start and newest progress are
// applied in m.parkMu, so a later progress waits for them and lands after.
func (m *Manager) resolveParked(handle string, p *managedPage, openers ...string) {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
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
	target := m.fateTarget(fate)
	for _, d := range popup.downloads {
		if target == nil || !m.admitTo(target, d.start) {
			log.Printf("browser: download %s of popup %s is listed in no tab: no page that opened the popup is open", d.start.ID, handle)
			m.refuseParked(d)
			continue
		}
		if d.newest != nil {
			m.applyDownloadProgress(*d.newest)
		}
	}
}

// refuseParked ends a parked download no tab took: its engine cancels a
// running one, and a staged file goes.
func (m *Manager) refuseParked(d *parkedDownload) {
	if d.newest == nil || d.newest.State == downloadInProgress {
		m.mu.Lock()
		var profile engineProfile
		for _, scope := range m.scopes {
			if scope.profile.Handle() == d.start.Profile {
				profile = scope.profile
				break
			}
		}
		m.mu.Unlock()
		// With its profile gone, so is the browser that ran it.
		if profile != nil {
			// Off the caller's locks: a cancel may wait on the engine's UI
			// thread.
			go func() {
				if err := profile.CancelDownload(d.start.ID); err != nil && !errors.Is(err, errDownloadEnded) {
					log.Printf("browser: cancel download %s listed in no tab: %v", d.start.ID, err)
				}
			}()
		}
	}
	if m.downloadsStagedHere() {
		m.releaseStagedDownload(d.start.ID)
	}
}

// fateTarget answers the page that takes a popup's downloads: its tab when
// listed, else its nearest listed opener, or nil.
func (m *Manager) fateTarget(fate popupFate) *managedPage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fate.listed != "" {
		return m.listedPageLocked(fate.listed)
	}
	for _, opener := range fate.openers {
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

// dropParked refuses every parked download and forgets every fate, for an
// engine that stopped.
func (m *Manager) dropParked() {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
	for handle, popup := range m.parked {
		popup.timer.Stop()
		for _, d := range popup.downloads {
			log.Printf("browser: download %s of popup %s is listed in no tab: the browser closed", d.start.ID, handle)
			if m.downloadsStagedHere() {
				m.releaseStagedDownload(d.start.ID)
			}
		}
	}
	m.parked, m.parkedIDs, m.popupFates = nil, nil, nil
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
