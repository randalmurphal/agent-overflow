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
	// handOver places download id's finished file where the Manager names
	// it (downloadFileOf), never replacing a file, and answers that path.
	handOver(id downloadID) (string, error)
	// withdraw removes the file handOver placed for download id, if it is
	// still there: the Manager did not take it.
	withdraw(id downloadID)
	// empty removes everything but the files of keep, whose ends are being
	// reported.
	empty(keep map[downloadID]bool)
	// close removes everything left, once nothing writes or reports any of
	// it.
	close()
}

// downloadRoom is the directory one Chromium process writes its downloads
// into: a directory of its own inside the profile's download directory.
// room holds the room itself open, so everything removed in it is removed
// in that directory whatever its name names meanwhile, and the room's own
// entry goes only while that name is still the room, as held, and empty
// (removeHeldDir). parent holds the profile's download directory, where a
// download the Manager admitted that finishes is placed under the name the
// Manager gave it, never replacing a file (handOver). Nothing else in the room is ever the
// Manager's, so whatever is left once the process has exited is removed
// with the room (close), including a download whose start no handler
// processed.
type downloadRoom struct {
	logf func(string, ...any)
	// dir is the profile's download directory, which parent holds, and name
	// the room in it, which room holds and held identifies.
	dir    string
	parent *os.Root
	name   string
	room   *os.Root
	held   os.FileInfo

	// mu orders the room's file work against close, which lets the roots
	// go. handed is each file handOver placed, as placed.
	mu     sync.Mutex
	closed bool
	handed map[downloadID]os.FileInfo
}

// openDownloadRoom makes a new room for one browser process in parent,
// the profile's download directory held open (profileOptions.
// OpenDownloadDir), named dir, and takes parent over. It holds only the
// directory it made, found empty (openMadeDir), and refuses the launch
// unless the path Chromium will be told, the room's name under dir,
// reaches the room held (nameReaches): a directory moved in at a name on
// that path would send Chromium's writes where AO neither holds nor
// measures them.
func openDownloadRoom(parent *os.Root, dir string, logf func(string, ...any)) (*downloadRoom, error) {
	name := "chromium-" + rand.Text()
	if err := parent.Mkdir(name, 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("browser: create the browser's download directory: %w", err), parent.Close())
	}
	room, held, err := openMadeDir(parent, dir, name)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("browser: open the browser's download directory: %w", err), parent.Close())
	}
	if err := nameReaches(filepath.Join(dir, name), held); err != nil {
		return nil, errors.Join(fmt.Errorf("browser: the browser is not launched: %w", err), room.Close(), removeMadeIn(parent, name, held), parent.Close())
	}
	return &downloadRoom{logf: logf, dir: dir, parent: parent, name: name, room: room, held: held, handed: map[downloadID]os.FileInfo{}}, nil
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
	for _, file := range chromiumDownloadNames(name) {
		if err := r.room.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.logf("browser: remove the file of download %s: %v", id, err)
		}
	}
}

// handOver places the regular file the room holds under id's name in the
// profile's download directory, never replacing a file there
// (placeNoReplace), and only while the room's path still reaches that file,
// as placing by name needs. The room's own name for it goes after.
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
	held, err := r.room.Lstat(name)
	if err != nil {
		return "", fmt.Errorf("browser: read the finished download %s: %w", id, err)
	}
	if !held.Mode().IsRegular() {
		return "", fmt.Errorf("browser: the file of download %s is not a regular file", id)
	}
	source := filepath.Join(r.path(), name)
	if current, err := os.Lstat(source); err != nil || !os.SameFile(held, current) {
		return "", fmt.Errorf("browser: the browser's download directory %s was moved or replaced", r.path())
	}
	if _, err := r.parent.Lstat(name); err == nil {
		return "", fmt.Errorf("browser: a file is already at %s, so download %s is not saved", filepath.Join(r.dir, name), id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("browser: read %s: %w", filepath.Join(r.dir, name), err)
	}
	path, placed, err := placeNoReplace(placeSource{path: source, info: held, dir: r.room}, r.dir, "the download directory", func(int) string { return name })
	if err != nil {
		return "", err
	}
	if err := r.room.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.logf("browser: remove the room's name for download %s: %v", id, err)
	}
	r.handed[id] = placed
	return path, nil
}

// withdraw removes the file handOver placed only while its name still
// holds that file.
func (r *downloadRoom) withdraw(id downloadID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	moved, ok := r.handed[id]
	delete(r.handed, id)
	if r.closed || !ok {
		return
	}
	name, err := downloadFileName(id)
	if err != nil {
		r.logf("%v", err)
		return
	}
	current, err := r.parent.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case err != nil:
		r.logf("browser: read the file of download %s: %v", id, err)
		return
	case !os.SameFile(moved, current):
		return
	}
	if err := r.parent.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.logf("browser: remove the file of download %s no page took: %v", id, err)
	}
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
	r.removeEntriesLocked(kept)
}

// removeEntriesLocked removes everything in the room but kept, through the
// room's own handle. The caller holds r.mu.
func (r *downloadRoom) removeEntriesLocked(kept map[string]bool) {
	dir, err := r.room.Open(".")
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
		if err := r.room.RemoveAll(entry.Name()); err != nil {
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
	r.removeEntriesLocked(nil)
	if err := r.room.Close(); err != nil {
		r.logf("browser: close the browser's download directory: %v", err)
	}
	if _, err := removeHeldDir(r.path(), r.held); err != nil {
		r.logf("browser: remove the browser's download directory %s: %v", r.path(), err)
	}
	if err := r.parent.Close(); err != nil {
		r.logf("browser: close the download directory: %v", err)
	}
}
