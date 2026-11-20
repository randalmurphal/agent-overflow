package browser

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// downloadFiles is what Chromium writes for the downloads of one browser
// process, as the download tracker settles it.
type downloadFiles interface {
	// discard removes the files Chromium writes download id under.
	discard(id downloadID)
	// handOver moves download id's finished file to where the Manager names
	// it (downloadFileOf) and answers that path.
	handOver(id downloadID) (string, error)
	// empty removes everything but the files of keep, whose ends are being
	// reported.
	empty(keep map[downloadID]bool)
	// close removes everything left, once nothing writes or reports any of
	// it.
	close()
}

// downloadRoom is the directory one Chromium process writes its downloads
// into: a directory of its own inside the profile's download directory,
// which root holds open, so every removal and move happens inside that
// directory whatever its path names meanwhile. A download the Manager
// admitted that finishes is moved out to the name the Manager gave it
// (handOver); nothing else in the room is ever the Manager's, so whatever
// is left once the process has exited is removed with the room (close),
// including a download whose start no handler processed.
type downloadRoom struct {
	logf func(string, ...any)
	// dir is the profile's download directory, which root holds, and name
	// the room in it.
	dir  string
	root *os.Root
	name string

	// mu orders the room's file work against close, which lets root go.
	mu     sync.Mutex
	closed bool
}

// openDownloadRoom makes a new room in dir, made if it is missing, for one
// browser process.
func openDownloadRoom(dir string, logf func(string, ...any)) (*downloadRoom, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("browser: create the download directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("browser: open the download directory: %w", err)
	}
	name := "chromium-" + rand.Text()
	if err := root.Mkdir(name, 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("browser: create the browser's download directory: %w", err), root.Close())
	}
	return &downloadRoom{logf: logf, dir: dir, root: root, name: name}, nil
}

// path is the room's directory, where Chromium is told to write.
func (r *downloadRoom) path() string { return filepath.Join(r.dir, r.name) }

// downloadFileName is the file name Chromium gives download id, which it
// names by its GUID (allowAndName), refused when it is not one plain name.
func downloadFileName(id downloadID) (string, error) {
	name := string(id)
	if name == "" || name == "." || name != filepath.Base(name) || !filepath.IsLocal(name) {
		return "", fmt.Errorf("browser: download id %q is not a file name", id)
	}
	return name, nil
}

func (r *downloadRoom) discard(id downloadID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	name, err := downloadFileName(id)
	if err != nil {
		r.logf("%v", err)
		return
	}
	for _, file := range chromiumDownloadNames(filepath.Join(r.name, name)) {
		if err := r.root.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.logf("browser: remove the file of download %s: %v", id, err)
		}
	}
}

func (r *downloadRoom) handOver(id downloadID) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", fmt.Errorf("browser: the download directory of download %s is gone", id)
	}
	name, err := downloadFileName(id)
	if err != nil {
		return "", err
	}
	if err := r.root.Rename(filepath.Join(r.name, name), name); err != nil {
		return "", fmt.Errorf("browser: move the finished download %s: %w", id, err)
	}
	return filepath.Join(r.dir, name), nil
}

func (r *downloadRoom) empty(keep map[downloadID]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	kept := make(map[string]bool, 2*len(keep))
	for id := range keep {
		if name, err := downloadFileName(id); err == nil {
			for _, file := range chromiumDownloadNames(name) {
				kept[file] = true
			}
		}
	}
	dir, err := r.root.Open(r.name)
	if err != nil {
		r.logf("browser: open the browser's download directory: %v", err)
		return
	}
	entries, err := dir.ReadDir(-1)
	if closeErr := dir.Close(); closeErr != nil {
		r.logf("browser: close the browser's download directory: %v", closeErr)
	}
	if err != nil {
		r.logf("browser: list the browser's download directory: %v", err)
	}
	for _, entry := range entries {
		if kept[entry.Name()] {
			continue
		}
		if err := r.root.RemoveAll(filepath.Join(r.name, entry.Name())); err != nil {
			r.logf("browser: remove %s from the browser's download directory: %v", entry.Name(), err)
		}
	}
}

func (r *downloadRoom) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if err := r.root.RemoveAll(r.name); err != nil {
		r.logf("browser: remove the browser's download directory %s: %v", r.path(), err)
	}
	if err := r.root.Close(); err != nil {
		r.logf("browser: close the download directory: %v", err)
	}
}
