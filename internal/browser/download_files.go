package browser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// A page of a workspace may open that workspace's finished downloads in the
// user's Downloads folder (authorizeFile): exactly the files of the finished
// rows the Manager still lists, in any thread's pane list or any live page's
// browser_downloads log, each only while it is still the file the download
// wrote (sameDownloadedFile), and never the folder. The rows are the one
// record: a row that is listed is openable, and one that is gone is not.

// engineSavedDownload checks the file an engine that saves downloads itself
// reports it saved: an absolute path naming a regular file, not a link, and
// of the size the engine reported when it reported one. It answers the path
// with its folder's links resolved, the form authorizeFile compares, and the
// file's identity, taken without following a link, which is what makes it
// openable.
//
// The limit: WebView2 reports only a path, so a regular file of the same
// size that replaced the saved one before the launcher's completion report
// was handled is taken as the download. Making that swap needs write access
// to the user's Downloads folder.
func engineSavedDownload(path string, size int64) (string, os.FileInfo, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("browser: the browser reported the download saved at %q, which is not a file path", path)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", nil, fmt.Errorf("browser: the downloaded file's folder: %w", err)
	}
	path = filepath.Join(dir, filepath.Base(path))
	file, err := os.Lstat(path)
	if err != nil {
		return "", nil, fmt.Errorf("browser: the downloaded file: %w", err)
	}
	if file.Mode()&os.ModeSymlink != 0 {
		return path, nil, errors.New("the saved file is a link, not the file the download wrote, so it is not opened from here")
	}
	if !file.Mode().IsRegular() {
		return path, nil, errors.New("the saved file is not a regular file, so it is not opened from here")
	}
	if size > 0 && file.Size() != size {
		return path, nil, fmt.Errorf("the saved file holds %d bytes, not the %d the download wrote, so it is not opened from here", file.Size(), size)
	}
	return path, file, nil
}

// listedDownloadAllows reports whether path, links resolved, is the file of
// a finished download of the canonical workspace that the Manager still
// lists, and is still that file.
func (m *Manager) listedDownloadAllows(workspace, path string) bool {
	var listed []os.FileInfo
	// The lists of the threads' current lifetimes: a closed thread's list
	// went with it.
	m.mu.Lock()
	m.downloadsMu.Lock()
	for _, tb := range m.threads {
		if tb.downloads == nil {
			continue
		}
		for _, item := range tb.downloads.items {
			if item.State == downloadStateCompleted && item.workspace == workspace && item.path == path && item.file != nil {
				listed = append(listed, item.file)
			}
		}
	}
	m.downloadsMu.Unlock()
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.access.Workspace != workspace {
				continue
			}
			p.downloadMu.Lock()
			for _, entry := range p.downloads {
				if entry.State == downloadStateCompleted && entry.Path == path && entry.file != nil {
					listed = append(listed, entry.file)
				}
			}
			p.downloadMu.Unlock()
		}
	}
	m.mu.Unlock()
	if len(listed) == 0 {
		return false
	}
	current, err := os.Stat(path)
	if err != nil {
		return false
	}
	for _, file := range listed {
		if sameDownloadedFile(file, current) {
			return true
		}
	}
	return false
}
