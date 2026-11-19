package browser

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/browser/quota"
)

// A download's admission to its page: the caps, its row and its reservation.

// downloadStarted admits a download into the log of the page whose frame
// it began in. It refuses it, and the engine cancels the download, when
// the page left or, on an engine whose downloads are AO's, the download is
// over quota: whether it may consume workspace and artifact bytes is
// policy, and so is the sanitized name it will eventually be renamed to.
// The quota is the page's workspace's (workspaceAuthority), whichever jar
// the page is in. A frame no managed page owns is downloadUnowned. A
// refused staged download's file goes.
func (m *Manager) downloadStarted(event downloadStart) downloadAdmission {
	admission := m.admitDownload(event)
	if admission != downloadAdmitted && m.downloadsStagedHere() {
		m.releaseStagedDownload(event.ID)
	}
	return admission
}

func (m *Manager) admitDownload(event downloadStart) downloadAdmission {
	// The address is display data from here, bounded once as it enters, in
	// a copy of its own, before anything parks or lists it.
	event.URL = strings.Clone(truncateUTF8(event.URL, maxBrowserURLBytes))
	p, _ := m.pageForFrame(event.Frame)
	switch {
	case p != nil:
		return admission(m.admitTo(p, event, nil))
	case event.Popup != "":
		// A popup not listed yet: its fate lists it (popup_adoption.go).
		return admission(m.parkDownload(event))
	}
	return downloadUnowned
}

func admission(admitted bool) downloadAdmission {
	if admitted {
		return downloadAdmitted
	}
	return downloadRefused
}

// admitTo lists a starting download in p. A parked download's listing
// (parked set) takes its reservation over.
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
	case parked != nil:
		// Its listing moves its reservation to its row: its bytes stay held,
		// against the workspace they were taken from.
		entry.reservation = quota.Take(&parked.reservation)
	case m.downloadsToArtifacts():
		reservation, err := m.reserveDownload(authority)
		if err != nil {
			entry.State, entry.Error = downloadStateCanceled, err.Error()
		}
		entry.reservation = reservation
	}
	p.downloads = append(p.downloads, entry)
	note := m.noteDownloadLocked(p, &p.downloads[len(p.downloads)-1], source)
	p.signalDownloadLocked()
	p.downloadMu.Unlock()
	m.publishDownloads(note)
	return entry.State != downloadStateCanceled
}

// errNoWorkspace refuses a download AO would keep that no workspace owns.
var errNoWorkspace = errors.New("the download belongs to no workspace")

// reserveDownload holds maxDownloadBytes of the workspace's download quota
// and the artifact root's, both or neither.
func (m *Manager) reserveDownload(authority *workspaceAuthority) (*quota.Reservation, error) {
	if authority == nil {
		return nil, errNoWorkspace
	}
	return m.reserveArtifacts(maxDownloadBytes, authority.downloadBytes)
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
func (m *Manager) releaseDownloadLocked(_ *managedPage, entry *DownloadInfo) {
	quota.Take(&entry.reservation).Release()
}
