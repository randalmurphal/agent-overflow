package browser

import (
	"errors"
	"fmt"
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
	p, scope := m.pageForFrame(event.Frame)
	switch {
	case p != nil:
		return m.admitTo(p, event, m.downloadFileOf(scope, event.ID), nil)
	case event.Popup != "":
		// A popup not listed yet: its fate lists it (popup_adoption.go).
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
	source := p.cachedInfo().URL
	if source == "" || source == "about:blank" {
		source = event.URL
	}
	if m.downloadAdmitting != nil {
		m.downloadAdmitting()
	}
	held := p.downloads.hold()
	if held.ended() {
		// The page left after it was looked up; nothing would settle this.
		held.release()
		return downloadRefused
	}
	entry := DownloadInfo{
		ID: event.ID, URL: event.URL,
		SuggestedName: safeArtifactName(event.SuggestedName, "download"),
		State:         downloadStateRunning, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		key: fmt.Sprintf("d%d", m.downloadKeys.Add(1)),
	}
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
			entry.State, entry.Error = downloadStateCanceled, err.Error()
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
