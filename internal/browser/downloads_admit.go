package browser

import (
	"fmt"
	"slices"
	"time"
)

// A download's admission to its page: the caps, its row and its reservation.

// downloadStarted admits a download into the log of the page whose frame
// it began in. It answers false, and the engine cancels the download, when
// no managed page owns that frame or, on an engine whose downloads are AO's,
// the download is over quota: whether it may consume workspace and artifact
// bytes is policy, and so is the sanitized name it will eventually be
// renamed to. The quota is the page's workspace's (workspaceAuthority),
// whichever jar the page is in. A refused staged download's file goes.
func (m *Manager) downloadStarted(event downloadStart) bool {
	admitted := m.admitDownload(event)
	if !admitted && m.downloadsStagedHere() {
		m.releaseStagedDownload(event.ID)
	}
	return admitted
}

func (m *Manager) admitDownload(event downloadStart) bool {
	p, _ := m.pageForFrame(event.Frame)
	if p == nil {
		if event.Popup != "" {
			// A popup not listed yet: its fate lists it (popup_adoption.go).
			return m.parkDownload(event)
		}
		return false
	}
	return m.admitTo(p, event, nil)
}

// admitTo lists a starting download in p. A parked download's listing
// (parked set) takes its reservation over, moved to p's workspace if that
// is another one's, or refuses it there.
func (m *Manager) admitTo(p *managedPage, event downloadStart, parked *parkedDownload) bool {
	authority := p.authority
	source := p.cachedInfo().URL
	if source == "" || source == "about:blank" {
		source = event.URL
	}
	if m.downloadAdmitting != nil {
		m.downloadAdmitting()
	}
	p.downloadMu.Lock()
	if p.downloadsEnded {
		// The page left after it was looked up; nothing would settle this.
		p.downloadMu.Unlock()
		return false
	}
	p.downloadSeq++
	entry := DownloadInfo{
		Sequence: p.downloadSeq, ID: event.ID, URL: event.URL,
		SuggestedName: safeArtifactName(event.SuggestedName, "download"),
		State:         downloadStateRunning, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		key: fmt.Sprintf("d%d", m.downloadKeys.Add(1)),
	}
	switch {
	case parked != nil && parked.authority != nil:
		m.takeParkedReservation(authority, parked, &entry)
	case m.downloadsToArtifacts():
		entry.reservedBytes = maxDownloadBytes
		m.reserveDownloadLocked(authority, &entry)
	}
	p.downloads = append(p.downloads, entry)
	note := m.noteDownloadLocked(p, &p.downloads[len(p.downloads)-1], source)
	p.signalDownloadLocked()
	p.downloadMu.Unlock()
	m.publishDownloads(note)
	return entry.State != downloadStateCanceled
}

// reserveDownloadLocked reserves a new download's bytes against its
// workspace's quota and the artifact root's, or settles entry as canceled
// over quota. The caller holds the page's downloadMu.
func (m *Manager) reserveDownloadLocked(authority *workspaceAuthority, entry *DownloadInfo) {
	if reason, ok := m.reserveDownloadBytes(authority); !ok {
		entry.State, entry.Error, entry.reservedBytes = downloadStateCanceled, reason, 0
	}
}

// reserveDownloadBytes reserves maxDownloadBytes against the workspace's
// quota and the artifact root's, both or neither, answering why not.
func (m *Manager) reserveDownloadBytes(authority *workspaceAuthority) (string, bool) {
	if authority == nil || !authority.reserveDownload(maxDownloadBytes) {
		return fmt.Sprintf("workspace downloads exceed %d bytes", maxWorkspaceDownloadBytes), false
	}
	if !m.reserveArtifacts(maxDownloadBytes) {
		authority.downloadBytes.Add(-maxDownloadBytes)
		return fmt.Sprintf("browser artifacts exceed %d bytes", maxArtifactRootBytes), false
	}
	return "", true
}

// takeParkedReservation moves a parked download's reservation to entry, its
// row in a page of authority, or, when authority is another workspace that
// has no room for it, returns it and settles entry as canceled over quota.
// The caller holds the page's downloadMu and m.parkMu.
func (m *Manager) takeParkedReservation(authority *workspaceAuthority, parked *parkedDownload, entry *DownloadInfo) {
	if parked.authority != authority {
		if authority == nil || !authority.reserveDownload(maxDownloadBytes) {
			m.releaseParked(parked)
			entry.State, entry.Error = downloadStateCanceled, fmt.Sprintf("workspace downloads exceed %d bytes", maxWorkspaceDownloadBytes)
			return
		}
		parked.authority.downloadBytes.Add(-maxDownloadBytes)
	}
	parked.authority = nil
	entry.reservedBytes = maxDownloadBytes
}

// trimDownloadsLocked numbers the entries that settled since it last ran
// and drops the earliest settled beyond maxPageDownloads, so the entry that
// just settled stays for its waiter however many run. A running one stays:
// its progress, its reservation and its cancel all need it, and the quotas
// bound how many can run. The caller holds p.downloadMu.
func (p *managedPage) trimDownloadsLocked() {
	settled := 0
	for i := range p.downloads {
		entry := &p.downloads[i]
		if entry.State == downloadStateRunning {
			continue
		}
		if entry.settled == 0 {
			p.downloadSettles++
			entry.settled = p.downloadSettles
		}
		settled++
	}
	if settled <= maxPageDownloads {
		return
	}
	stamps := make([]uint64, 0, settled)
	for _, entry := range p.downloads {
		if entry.settled != 0 {
			stamps = append(stamps, entry.settled)
		}
	}
	slices.Sort(stamps)
	cutoff := stamps[settled-maxPageDownloads-1]
	kept := make([]DownloadInfo, 0, len(p.downloads)-(settled-maxPageDownloads))
	for _, entry := range p.downloads {
		if entry.settled == 0 || entry.settled > cutoff {
			kept = append(kept, entry)
		}
	}
	p.downloads = kept
}

// downloadLocked answers the page's entry for an engine download id, nil for
// none. The pointer is valid until p.downloadMu is released. The caller holds
// p.downloadMu.
func (p *managedPage) downloadLocked(id string) *DownloadInfo {
	for i := len(p.downloads) - 1; i >= 0; i-- {
		if p.downloads[i].ID == id {
			return &p.downloads[i]
		}
	}
	return nil
}

// releaseDownloadLocked returns a download's whole reservation, for one that
// ends with no file kept. The caller holds p.downloadMu.
func (m *Manager) releaseDownloadLocked(p *managedPage, entry *DownloadInfo) {
	if p.authority != nil && entry.reservedBytes > 0 {
		p.authority.downloadBytes.Add(-entry.reservedBytes)
		m.settleArtifactReservation(entry.reservedBytes, 0)
		entry.reservedBytes = 0
	}
}
