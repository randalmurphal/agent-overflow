package browser

import (
	"errors"
	"fmt"
	"log"
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
	event = boundedStart(event)
	p, scope := m.pageForFrame(event.Frame)
	switch {
	case p != nil:
		return m.admitTo(p, event, m.downloadFileOf(scope, event.ID), nil)
	case event.Popup != "":
		// A popup not listed yet: its fate lists it (popup_parked.go).
		return m.parkDownload(event)
	}
	return downloadUnowned
}

// admitTo lists a starting download in p, its owner, with file, the file
// its engine writes it into. It answers downloadRefused when p left (its
// log ended) or, on an engine whose downloads are AO's, p's workspace
// refuses the reservation, which the row then says. A parked download's
// listing (parked set) takes its file and reservation over.
func (m *Manager) admitTo(p *managedPage, event downloadStart, file downloadFile, parked *parkedDownload) downloadAdmission {
	authority := p.authority
	source := downloadSource(p, event)
	if m.downloadAdmitting != nil {
		m.downloadAdmitting()
	}
	held := p.downloads.hold()
	if held.ended() {
		// The page left after it was looked up; nothing would settle this.
		held.release()
		return downloadRefused
	}
	entry := m.downloadEntry(event)
	switch {
	case parked != nil:
		// Its listing moves its file and reservation to its row: its bytes
		// stay held, against the workspace they were taken from.
		entry.reservation = quota.Take(&parked.reservation)
		entry.source, parked.file = parked.file, downloadFile{}
	case m.downloadsToArtifacts():
		reservation, err := m.reserveDownload(authority)
		if err != nil {
			// Refused: its engine cancels it and removes its file.
			if settleErr := entry.settle(downloadStateCanceled); settleErr != nil {
				log.Printf("browser: %v", settleErr)
			}
			entry.Error = err.Error()
		} else {
			entry.source = file
		}
		entry.reservation = reservation
	default:
		entry.source = file
	}
	note := m.noteDownloadStartLocked(p, held.add(entry), source)
	held.release()
	m.publishDownloads(note)
	if entry.State == downloadStateCanceled {
		return downloadRefused
	}
	return downloadAdmitted
}

// boundedStart is event with its address, display data from here, bounded
// once as it enters, in a copy of its own, before anything parks or lists
// it.
func boundedStart(event downloadStart) downloadStart {
	event.URL = strings.Clone(truncateUTF8(event.URL, maxBrowserURLBytes))
	return event
}

// downloadSource is the Source a download starting in p lists: the page's
// address, or the download's own while the page has none.
func downloadSource(p *managedPage, event downloadStart) string {
	if source := p.cachedInfo().URL; source != "" && source != "about:blank" {
		return source
	}
	return event.URL
}

// downloadEntry is the running row of a download starting with event,
// numbered for its thread's list.
func (m *Manager) downloadEntry(event downloadStart) DownloadInfo {
	return DownloadInfo{
		ID: string(event.ID), URL: event.URL,
		SuggestedName: safeArtifactName(event.SuggestedName, "download"),
		State:         downloadStateRunning, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		key:         DownloadKey(fmt.Sprintf("d%d", m.downloadKeys.Add(1))),
		endReported: event.EndReported,
	}
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
