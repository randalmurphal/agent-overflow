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

// A download that ends keeping no file AO had its engine write: the file
// goes first, then the quota the download held settles with what is left
// of it on disk, so bytes a removal could not free stay charged.

// downloadFile is the file AO had an engine write one download into, named
// once as the download is admitted (downloadFileOf) and kept with its row
// or parked download, so whatever ends the download removes that file and
// no other.
type downloadFile struct {
	kind downloadFileKind
	// id is a staged download's engine id, by which the staging record
	// holds its file (stageDownload): the engine names the file after the
	// download starts.
	id downloadID
	// path is an artifact's file in its jar's download directory.
	path string
}

type downloadFileKind int

const (
	// noDownloadFile: the engine saves to the user's Downloads folder and
	// removes its own partial file, or no jar directory held it.
	noDownloadFile downloadFileKind = iota
	// stagedDownloadFile: a staged file, which holds no quota.
	stagedDownloadFile
	// artifactDownloadFile: an artifact in its jar's download directory,
	// which the download's reservation pays for.
	artifactDownloadFile
)

// downloadFileOf names the file the engine writes download id into, in the
// jar scope it is written in. The one place a download's file is derived.
func (m *Manager) downloadFileOf(scope *jarScope, id downloadID) downloadFile {
	switch m.engine.DownloadSaving() {
	case downloadsStaged:
		return downloadFile{kind: stagedDownloadFile, id: id}
	case downloadsAsArtifacts:
		if scope == nil || scope.downloadDir == "" {
			return downloadFile{}
		}
		return downloadFile{kind: artifactDownloadFile, path: filepath.Join(scope.downloadDir, filepath.Base(string(id)))}
	case downloadsSavedByEngine:
		return downloadFile{}
	}
	panic(fmt.Sprintf("browser: download saving %d", m.engine.DownloadSaving()))
}

// downloadDiscard is a download's end that keeps no file, owning both its
// file and its reservation from the hold that settled it until finishDiscard.
type downloadDiscard struct {
	id          downloadID
	file        downloadFile
	reservation *quota.Reservation
}

// discard takes the row's file and reservation for its end that keeps
// neither, leaving the row none. The caller holds the row's log.
func (e *DownloadInfo) discard() downloadDiscard {
	file := e.source
	e.source = downloadFile{}
	return downloadDiscard{id: downloadID(e.ID), file: file, reservation: quota.Take(&e.reservation)}
}

// discard takes the parked download's file and reservation for its end
// that keeps neither. The caller holds m.parkMu.
func (d *parkedDownload) discard() downloadDiscard {
	file := d.file
	d.file = downloadFile{}
	return downloadDiscard{id: d.start.ID, file: file, reservation: quota.Take(&d.reservation)}
}

// finishDiscard removes a discarded download's file, then settles its
// reservation with the bytes still on disk: none once the file is gone,
// the file's size when its removal failed, which is logged. Call it
// holding no lock, once the engine has stopped writing the file.
func (m *Manager) finishDiscard(d downloadDiscard) {
	remaining, err := m.removeDownloadFile(d.file)
	if err != nil {
		log.Printf("browser: remove the file of download %s: %v; its %d bytes stay charged", d.id, err, remaining)
	}
	d.reservation.Settle(remaining)
}

// removeDownloadFile removes f and answers the bytes left of it on disk.
// A staged file holds no quota, and its release logs its own failure.
func (m *Manager) removeDownloadFile(f downloadFile) (int64, error) {
	switch f.kind {
	case noDownloadFile:
		return 0, nil
	case stagedDownloadFile:
		m.releaseStagedDownload(f.id)
		return 0, nil
	case artifactDownloadFile:
		info, statErr := os.Lstat(f.path)
		if errors.Is(statErr, fs.ErrNotExist) {
			return 0, nil
		}
		err := os.Remove(f.path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		if statErr != nil {
			// Its size is unknown: it keeps what it reserved.
			return maxDownloadBytes, errors.Join(err, statErr)
		}
		return info.Size(), err
	}
	panic(fmt.Sprintf("browser: download file kind %d", f.kind))
}
