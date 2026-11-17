package browser

import (
	"fmt"
	"os"
	"path/filepath"
)

// The files pane downloads saved into the user's Downloads folder, which a
// page of the workspace they downloaded in may open (authorizeFile): those
// files alone, never the folder, and each only while it is still the file
// the download wrote (sameDownloadedFile). The record is in memory, bounded
// per workspace and in workspaces, and outlives the pages and the threads'
// lists so a closed tab or a cleared list does not take the allowance with
// it. It never deletes a file.

const (
	// maxRecordedDownloads bounds one workspace's record; the oldest go first.
	maxRecordedDownloads = 100
	// maxRecordedWorkspaces bounds the workspaces with a record; the one
	// whose newest download is oldest goes first.
	maxRecordedWorkspaces = 32
)

type recordedDownload struct {
	path string
	file os.FileInfo
}

type workspaceDownloads struct {
	// files is oldest first.
	files []recordedDownload
	// latest orders the workspaces by their newest record.
	latest uint64
}

// savedDownload checks the file an engine reports it saved a download to:
// an absolute path to a regular file. It answers the path with its links
// resolved, the form authorizeFile compares, and the file as it is now.
func savedDownload(path string) (string, os.FileInfo, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("browser: the browser reported the download saved at %q, which is not a file path", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, fmt.Errorf("browser: the downloaded file: %w", err)
	}
	file, err := os.Stat(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("browser: the downloaded file: %w", err)
	}
	if !file.Mode().IsRegular() {
		return "", nil, fmt.Errorf("browser: the download saved %s, which is not a regular file", resolved)
	}
	return resolved, file, nil
}

// recordFinishedDownload adds a finished download's file to its canonical
// workspace's record. Takes m.downloadsMu.
func (m *Manager) recordFinishedDownload(workspace, path string, file os.FileInfo) {
	if workspace == "" {
		return
	}
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	if m.finishedDownloads == nil {
		m.finishedDownloads = make(map[string]*workspaceDownloads)
	}
	record := m.finishedDownloads[workspace]
	if record == nil {
		if len(m.finishedDownloads) >= maxRecordedWorkspaces {
			oldest := ""
			for key, other := range m.finishedDownloads {
				if oldest == "" || other.latest < m.finishedDownloads[oldest].latest {
					oldest = key
				}
			}
			delete(m.finishedDownloads, oldest)
		}
		record = &workspaceDownloads{}
		m.finishedDownloads[workspace] = record
	}
	m.recordSeq++
	record.latest = m.recordSeq
	record.files = append(record.files, recordedDownload{path: path, file: file})
	if excess := len(record.files) - maxRecordedDownloads; excess > 0 {
		record.files = append(record.files[:0:0], record.files[excess:]...)
	}
}

// recordedDownloadAllows reports whether path, links resolved, is a file
// one of the canonical workspace's pane downloads saved and is still that
// file.
func (m *Manager) recordedDownloadAllows(workspace, path string) bool {
	var recorded []os.FileInfo
	m.downloadsMu.Lock()
	if record := m.finishedDownloads[workspace]; record != nil {
		for _, entry := range record.files {
			if entry.path == path {
				recorded = append(recorded, entry.file)
			}
		}
	}
	m.downloadsMu.Unlock()
	if len(recorded) == 0 {
		return false
	}
	current, err := os.Stat(path)
	if err != nil {
		return false
	}
	for _, file := range recorded {
		if sameDownloadedFile(file, current) {
			return true
		}
	}
	return false
}
