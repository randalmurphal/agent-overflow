//go:build linux

package browser

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// linkFile is the link a placement makes (linkDownload): linkat following
// a final link, so a /proc/self/fd name links the open file itself.
func linkFile(from, to string) error {
	if err := unix.Linkat(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.AT_SYMLINK_FOLLOW); err != nil {
		return &os.LinkError{Op: "link", Old: from, New: to, Err: err}
	}
	return nil
}

// placeHeld is placeNoReplace's placement from the held source: it opens
// source in its held directory, checks the open file is source's, and
// links that open file, through its /proc/self/fd name, into dir opened
// as a directory, through that one's, under the first free name. The name
// it publishes then holds the bytes it checked whatever happens to
// source's name or dir's path meanwhile, and no private name is made. It
// answers errNoHeldLink where /proc does not show the open files or the
// filesystem refuses hard links, for placeNoReplace to place by name.
func placeHeld(source placeSource, dir, where string, name func(i int) string) (string, os.FileInfo, error) {
	// Non-blocking, so a FIFO swapped in at the name cannot hold the open.
	file, err := source.dir.OpenFile(filepath.Base(source.path), os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, fmt.Errorf("browser: open %s to save it: %w", source.path, err)
	}
	defer closeHeld(file)
	opened, err := file.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("browser: read %s to save it: %w", source.path, err)
	}
	if !os.SameFile(opened, source.info) {
		return "", nil, fmt.Errorf("browser: %s changed before the download was saved, so it is not saved", source.path)
	}
	target, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return "", nil, fmt.Errorf("browser: open %s %s: %w", where, dir, err)
	}
	defer closeHeld(target)
	from, into := fdPath(file), fdPath(target)
	if from == "" || into == "" {
		return "", nil, errNoHeldLink
	}
	for i := 0; i < maxNameVariants; i++ {
		public := name(i)
		held := filepath.Join(into, public)
		err := linkDownload(from, held)
		switch {
		case err == nil:
			// The name is the checked file's unless something replaced it
			// since the link; that stays, as it may not be AO's.
			placed, err := os.Lstat(held)
			if err != nil {
				return "", nil, fmt.Errorf("browser: save in %s: %w", where, err)
			}
			if !os.SameFile(placed, source.info) {
				return "", nil, fmt.Errorf("browser: %s changed as the download was saved there, so the download is not saved", filepath.Join(dir, public))
			}
			return filepath.Join(dir, public), source.info, nil
		case errors.Is(err, fs.ErrExist):
		case linksRefused(err):
			return "", nil, errNoHeldLink
		case errors.Is(err, fs.ErrNotExist):
			// The checked file has no name left (Linux links no file whose
			// last name went), or dir went.
			return "", nil, fmt.Errorf("browser: %s, or %s %s, went before the download was saved, so it is not saved: %w", source.path, where, dir, err)
		default:
			return "", nil, fmt.Errorf("browser: save in %s: %w", where, err)
		}
	}
	return "", nil, fmt.Errorf("browser: %d files named like %q are already in %s", maxNameVariants, name(0), dir)
}

// fdPath is the /proc/self/fd name of f, "" where /proc does not show it.
func fdPath(f *os.File) string {
	path := "/proc/self/fd/" + strconv.Itoa(int(f.Fd()))
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// closeHeld closes a file placeHeld held, logging a failure: the link is
// made or refused by then.
func closeHeld(f *os.File) {
	if err := f.Close(); err != nil {
		log.Printf("browser: close %s: %v", f.Name(), err)
	}
}
