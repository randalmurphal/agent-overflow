package browser

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"agent-overflow/internal/browser/quota"
)

// A download's progress applied to its row, and a finished download's publish.

func (m *Manager) downloadProgress(event downloadProgress) {
	m.parkMu.Lock()
	parked := m.parkProgressLocked(event)
	m.parkMu.Unlock()
	if !parked {
		m.applyDownloadProgress(event)
	}
}

// applyDownloadProgress applies a download's progress to its row.
func (m *Manager) applyDownloadProgress(event downloadProgress) {
	p, scope := m.pageForDownload(event.ID)
	if p == nil {
		return
	}
	held := p.downloads.hold()
	entry := held.entry(event.ID)
	// A settled download takes no further report: AO stopped it (a quota,
	// its size, the user, its page closing) or the engine already ended it.
	// One being published has had its finish.
	if entry == nil || entry.State != downloadStateRunning || entry.publishing {
		held.release()
		return
	}
	if event.CancelRefused {
		// The engine refused the cancel after taking it: the download runs
		// on, listed with the refusal, and the user may cancel again. A
		// later end reads as the engine's own, not the user's cancel.
		entry.cancelRequested = false
		entry.Error = event.Error
		note := m.noteDownloadLocked(p, entry)
		held.release()
		m.publishDownloads(note)
		return
	}
	if event.State != downloadInProgress {
		// A running row carries only a refused cancel's reason, which its
		// end replaces.
		entry.Error = ""
	}
	entry.Bytes = int64(event.Received)
	if event.Total > 0 {
		entry.TotalBytes = int64(event.Total)
	}
	saving := m.engine.DownloadSaving()
	// Over the cap: running, it is canceled; finished, which a popup's
	// download listed only at its finish can be (popup_adoption.go), its
	// file goes all the same.
	if saving == downloadsAsArtifacts && entry.Bytes > maxDownloadBytes && event.State != downloadCanceled {
		entry.State = downloadStateCanceled
		entry.Error = fmt.Sprintf("download exceeds %d bytes", maxDownloadBytes)
		m.releaseDownloadLocked(p, entry)
		note := m.noteDownloadLocked(p, entry)
		held.release()
		m.publishDownloads(note)
		if scope != nil {
			running := event.State == downloadInProgress
			go func() {
				if running {
					// One parked past the cap was canceled then.
					if err := scope.profile.CancelDownload(event.ID); err != nil && !errors.Is(err, errDownloadEnded) {
						log.Printf("browser: cancel download %s over its size cap: %v", event.ID, err)
					}
				}
				m.removePartialDownload(scope, event.ID)
			}()
		}
		return
	}
	if event.State == downloadCompleted && saving == downloadsStaged {
		m.publishFinishedDownload(p, held, entry, event)
		return
	}
	removePartial := false
	switch event.State {
	case downloadCompleted:
		entry.State = downloadStateCompleted
		if saving == downloadsSavedByEngine {
			// The engine saved it in the user's Downloads folder, where it
			// stays. Only the regular file the engine reported, as it is
			// now, is openable; anything else completes with the reason.
			if event.FilePath == "" && event.Error != "" {
				// Saved where the engine could not name or reach it.
				entry.Error = event.Error
				break
			}
			size := int64(event.Total)
			if size <= 0 {
				size = int64(event.Received)
			}
			path, file, err := engineSavedDownload(event.FilePath, size)
			entry.Path, entry.file = path, file
			if err != nil {
				entry.Error = err.Error()
			}
		} else if scope != nil && p.authority != nil {
			// The engine writes into its jar's directory; the finished file
			// moves to the page's workspace's.
			source := event.FilePath
			if source == "" {
				source = filepath.Join(scope.downloadDir, filepath.Base(event.ID))
			}
			if !pathInside(scope.downloadDir, source) {
				entry.State, entry.Error = downloadStateFailed, "browser returned a download path outside its artifact directory"
			} else if destination, file, moveErr := placeArtifact(source, p.authority.downloadDir, entry.SuggestedName); moveErr != nil {
				entry.State, entry.Error = downloadStateFailed, moveErr.Error()
			} else {
				entry.Path, entry.file = destination, file
			}
		}
		if entry.State == downloadStateCompleted {
			// The kept file holds its size of the quotas from here.
			quota.Take(&entry.reservation).Settle(entry.Bytes)
		} else {
			// Nothing is kept: the engine's file goes with its reservation.
			m.releaseDownloadLocked(p, entry)
			removePartial = true
		}
	case downloadCanceled:
		// An engine reports the cancel AO asked for and its own failures (a
		// dropped connection, a server error) the same way; only the first
		// is a cancel.
		if entry.cancelRequested || event.PageGone {
			entry.State = downloadStateCanceled
			if event.PageGone {
				entry.Error = event.Error
			}
			removePartial = true
		} else {
			entry.State, entry.Error = downloadStateFailed, event.Error
			if entry.Error == "" {
				entry.Error = "the download stopped before it finished"
			}
		}
		m.releaseDownloadLocked(p, entry)
		// A staged download that stopped, canceled or failed, leaves nothing
		// but its staged file, which goes.
		removePartial = removePartial || saving == downloadsStaged
	}
	note := m.noteDownloadLocked(p, entry)
	held.release()
	if removePartial {
		// What the engine wrote is a partial file.
		m.removePartialDownload(scope, event.ID)
	}
	m.publishDownloads(note)
}

// placeArtifact moves a finished artifact download from the file its engine
// wrote into dir under the first free variant of name, never replacing a
// file: the downloads of a workspace's jars finish on their own engine
// streams into one directory (placeNoReplace).
func placeArtifact(source, dir, name string) (string, os.FileInfo, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", nil, fmt.Errorf("browser: the downloaded file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("browser: the download's file %s is not a regular file", source)
	}
	path, file, err := placeNoReplace(source, info, dir, "the download directory", artifactNames(name))
	if err != nil {
		return "", nil, err
	}
	// A link leaves the engine's name for the file, which goes.
	if err := os.Remove(source); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("browser: remove the download's engine file %s: %v", source, err)
	}
	return path, file, nil
}

// removePartialDownload removes the file an engine writing into AO's
// directories was streaming a download into, once the download stopped:
// its staged file, or its handle-named artifact. An engine that saves to
// the user's Downloads folder itself removes its own partial file.
func (m *Manager) removePartialDownload(scope *jarScope, id string) {
	if m.downloadsStagedHere() {
		m.releaseStagedDownload(id)
		return
	}
	if scope == nil || scope.downloadDir == "" {
		return
	}
	if err := os.Remove(filepath.Join(scope.downloadDir, filepath.Base(id))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("browser: remove the partial download %s: %v", id, err)
	}
}

// publishFinishedDownload settles a staged download the engine finished:
// its file moves from the staging directory into the Downloads folder
// beside it (publishDownload) and the row completes with the published
// path, or fails with the reason, the staged file released either way. The
// row stays running while the file moves, outside the page's lock: a page
// that leaves meanwhile (pageLeftLocked) and a cancel
// (CancelThreadDownload) leave it to this, its one settlement, and Close
// waits for it (waitPublishes). The caller holds p's download log, which
// this releases.
func (m *Manager) publishFinishedDownload(p *managedPage, held *downloadLogHold, entry *DownloadInfo, event downloadProgress) {
	source := m.stagedPath(event.ID)
	failure := ""
	switch {
	case source == "":
		failure = "browser: the download has no staged file to save"
	case event.FilePath != "" && filepath.Clean(event.FilePath) != source:
		failure = "browser returned a download path other than the staged file it was given"
	}
	if failure != "" {
		entry.State, entry.Error = downloadStateFailed, failure
		note := m.noteDownloadLocked(p, entry)
		held.release()
		m.releaseStagedDownload(event.ID)
		m.publishDownloads(note)
		return
	}
	entry.publishing = true
	name := entry.SuggestedName
	m.beginPublish()
	held.release()

	path, file, err := publishDownload(source, name)
	m.releaseStagedDownload(event.ID)

	held = p.downloads.hold()
	// A running entry is never trimmed, so it is still there.
	entry = held.entry(event.ID)
	if entry != nil {
		entry.publishing = false
		if err != nil {
			entry.State, entry.Error = downloadStateFailed, err.Error()
		} else {
			entry.State, entry.Path, entry.file = downloadStateCompleted, path, file
		}
	}
	var note downloadNote
	if entry != nil {
		note = m.noteDownloadLocked(p, entry)
	}
	held.release()
	m.endPublish()
	if entry != nil {
		m.publishDownloads(note)
	}
}

// beginPublish and endPublish count the publishes in flight, which Close
// waits for (waitPublishes): a finished download is in the Downloads folder
// and its row settled before Close returns.
func (m *Manager) beginPublish() {
	m.publishMu.Lock()
	m.publishing++
	m.publishMu.Unlock()
}

func (m *Manager) endPublish() {
	m.publishMu.Lock()
	m.publishing--
	if m.publishing == 0 && m.publishIdle != nil {
		close(m.publishIdle)
		m.publishIdle = nil
	}
	m.publishMu.Unlock()
}

// waitPublishes returns once no publish is in flight.
func (m *Manager) waitPublishes() {
	m.publishMu.Lock()
	if m.publishing == 0 {
		m.publishMu.Unlock()
		return
	}
	if m.publishIdle == nil {
		m.publishIdle = make(chan struct{})
	}
	idle := m.publishIdle
	m.publishMu.Unlock()
	<-idle
}
