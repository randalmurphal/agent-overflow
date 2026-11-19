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
	if !event.State.valid() {
		log.Printf("browser: download %s reported with no state (%v); dropped", event.ID, event.State)
		return
	}
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
	if event.State == downloadCancelRefused {
		// The engine refused the cancel after taking it: the download runs
		// on, listed with the refusal, and the user may cancel again.
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
	if saving == downloadsAsArtifacts && entry.Bytes > maxDownloadBytes && !event.State.stopped() {
		entry.State = downloadStateCanceled
		entry.Error = fmt.Sprintf("download exceeds %d bytes", maxDownloadBytes)
		discard := entry.discard()
		note := m.noteDownloadLocked(p, entry)
		held.release()
		m.publishDownloads(note)
		// One parked past the cap was canceled then.
		cancel := event.State.running() && scope != nil
		go func() {
			if cancel {
				if err := scope.profile.CancelDownload(event.ID); err != nil && !errors.Is(err, errDownloadEnded) {
					log.Printf("browser: cancel download %s over its size cap: %v", event.ID, err)
				}
			}
			m.finishDiscard(discard)
		}()
		return
	}
	if event.State == downloadCompleted && saving == downloadsStaged {
		m.publishFinishedDownload(p, held, entry, event)
		return
	}
	// keep is false for an end that keeps no file: what the engine wrote,
	// staged or an artifact, is a partial file, which goes.
	keep := true
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
		} else if entry.source.kind == artifactDownloadFile && p.authority != nil {
			// The engine writes into its jar's directory; the finished file
			// moves to the page's workspace's.
			source := event.FilePath
			if source == "" {
				source = entry.source.path
			}
			if !pathInside(filepath.Dir(entry.source.path), source) {
				entry.State, entry.Error = downloadStateFailed, "browser returned a download path outside its artifact directory"
			} else if destination, file, moveErr := placeArtifact(source, p.authority.downloadDir, entry.SuggestedName, maxDownloadBytes); moveErr != nil {
				entry.State, entry.Error = downloadStateFailed, moveErr.Error()
			} else {
				entry.Path, entry.file = destination, file
			}
		}
		if entry.State == downloadStateCompleted {
			// The kept file holds its size on disk of the quotas from here,
			// whatever the engine reported receiving.
			kept := entry.Bytes
			if entry.file != nil {
				kept = entry.file.Size()
				entry.Bytes = kept
			}
			quota.Take(&entry.reservation).Settle(kept)
			entry.source = downloadFile{}
		} else {
			keep = false
		}
	case downloadCanceled:
		// The cancel the user asked for, which needs no reason.
		entry.State = downloadStateCanceled
	case downloadPageGone:
		// The cancel its page's end made, which says why.
		entry.State, entry.Error = downloadStateCanceled, event.Error
	case downloadFailed, downloadUntracked:
		// The engine's own end, or one it no longer follows, a cancel
		// pending or not: listed failed, with the engine's reason.
		entry.State, entry.Error = downloadStateFailed, event.Error
		if entry.Error == "" {
			entry.Error = "the download stopped before it finished"
		}
	case downloadInProgress, downloadCancelRefused:
	}
	if event.State.stopped() {
		keep = false
	}
	var discard downloadDiscard
	if !keep {
		discard = entry.discard()
	}
	note := m.noteDownloadLocked(p, entry)
	held.release()
	if !keep {
		// The engine reported its end, so it writes the file no more.
		m.finishDiscard(discard)
	}
	m.publishDownloads(note)
}

// placeArtifact moves a finished artifact download from the file its engine
// wrote into dir under the first free variant of name, never replacing a
// file: the downloads of a workspace's jars finish on their own engine
// streams into one directory (placeNoReplace). A file over limit bytes on
// disk is refused, whatever the engine reported.
func placeArtifact(source, dir, name string, limit int64) (string, os.FileInfo, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", nil, fmt.Errorf("browser: the downloaded file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("browser: the download's file %s is not a regular file", source)
	}
	if info.Size() > limit {
		return "", nil, fmt.Errorf("download exceeds %d bytes", limit)
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
		discard := entry.discard()
		note := m.noteDownloadLocked(p, entry)
		held.release()
		m.finishDiscard(discard)
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
		// Its staged file is released, published or not.
		entry.source = downloadFile{}
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
