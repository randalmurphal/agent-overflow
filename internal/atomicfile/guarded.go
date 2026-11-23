package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ReadWithStat reads path with the file's info as of the read, for a
// read-modify-write against another writer that may replace the file at
// any time (a provider CLI's own config). A missing file is (nil, nil,
// nil). A symbolic link to a missing file is an error that says so: a
// write could not create the file through the link (WriteIfUnchanged
// never replaces what is at the path when it read nothing there).
//
// The bytes and the info come from one open file, so they are one file's
// even when another writer renames a new version in meanwhile; the write
// then finds the path no longer holds that file. A writer that changes the
// open file in place during the read changes its size or time, and the
// read is retried a bounded number of times.
func ReadWithStat(path string) ([]byte, os.FileInfo, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		data, info, stable, err := readOpenFile(path)
		if err != nil || stable {
			return data, info, err
		}
	}
	return nil, nil, fmt.Errorf("read %s: file changed during read across %d attempts", path, maxAttempts)
}

// readOpenFile is one attempt of ReadWithStat: the bytes and info of the
// file path opens, and whether the file kept its size and time while it
// was read.
func readOpenFile(path string) (data []byte, info os.FileInfo, stable bool, err error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		link, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, nil, true, nil
		case err != nil:
			return nil, nil, false, fmt.Errorf("inspect %s: %w", path, err)
		case link.Mode()&os.ModeSymlink == 0:
			// A file appeared since the open: read again.
			return nil, nil, false, nil
		}
		return nil, nil, true, danglingLinkError(path)
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	// A read-only descriptor's close reports nothing the read did not.
	defer func() { _ = file.Close() }()
	if openedForRead != nil {
		openedForRead()
	}
	before, err := file.Stat()
	if err != nil {
		return nil, nil, false, fmt.Errorf("stat %s: %w", path, err)
	}
	data, err = io.ReadAll(file)
	if err != nil {
		return nil, nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, false, fmt.Errorf("re-stat %s: %w", path, err)
	}
	return data, before, sameVersion(before, after), nil
}

// danglingLinkError is the error for path, a symbolic link to a file that
// does not exist.
func danglingLinkError(path string) error {
	target, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("read the link %s: %w", path, err)
	}
	return fmt.Errorf("%s is a symbolic link to %s, which does not exist: create %s or remove the link", path, target, target)
}

// sameVersion reports whether two stats of one path describe the same
// version of the file: the same file (os.SameFile, so a rename of another
// file onto the path differs even with equal size and time), the same size
// and the same modification time.
func sameVersion(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// WriteIfUnchanged replaces path with data, as Write does, only while the
// file is still the version ReadWithStat answered: before, or no file when
// before is nil.
//
// It answers one of three outcomes:
//   - (false, nil): the file changed since the read, so nothing was
//     written; the caller reads again and retries. A file that appeared
//     since a read of none, or disappeared since a read of one, is a change.
//   - (false, err): nothing was written.
//   - (true, err): data is in place. A non-nil err is the directory sync
//     that failed after the rename, so a power loss may still roll the
//     rename back; the write itself happened and must be treated as done.
//
// This is best-effort detection of a writer that does not cooperate, not
// a guarantee. The new file is staged first and the check is the last
// step before the rename, but a writer that replaces the file between that
// check and the rename is still overwritten. When before is nil the rename
// cannot replace a file (ExclusiveRename), so that case has no
// such window. On Windows os.SameFile reads both identities from the path
// when compared, so the identity half of the check adds nothing there; the
// app runs on Windows through WSL, where this is Linux.
func WriteIfUnchanged(path string, data []byte, before os.FileInfo) (bool, error) {
	// A change already visible is refused before the file is staged, so a
	// caller retrying against a busy writer does not write and sync a whole
	// candidate each time. The check before the rename stays the real one.
	if changed, err := changedSince(path, before); err != nil || changed {
		return false, err
	}
	tmpPath, err := stageWrite(path, data)
	if err != nil {
		return false, err
	}
	published, err := publishIfUnchanged(tmpPath, path, before)
	if err != nil || !published {
		_ = os.Remove(tmpPath)
		return false, err
	}
	if err := syncPublished(filepath.Dir(path)); err != nil {
		return true, err
	}
	return true, nil
}

// Test seams: syncPublished is SyncDir, replaced to fail the sync after a
// write committed; stageWrite is stage, replaced to see whether a write
// staged its file; openedForRead, when set, runs just after ReadWithStat
// opened the file.
var (
	syncPublished = SyncDir
	stageWrite    = stage
	openedForRead func()
)

// changedSince reports whether path is no longer the version before
// names: a file that appeared since a read of none (anything at the path,
// a link included), or one that disappeared or was replaced since a read
// of one.
func changedSince(path string, before os.FileInfo) (bool, error) {
	if before == nil {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("atomicfile: recheck %s: %w", path, err)
		}
		return true, nil
	}
	current, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("atomicfile: recheck %s: %w", path, err)
	}
	return !sameVersion(before, current), nil
}

// publishIfUnchanged renames the staged tmpPath onto path when path is
// still the version before names, and reports whether it did.
func publishIfUnchanged(tmpPath, path string, before os.FileInfo) (bool, error) {
	if before == nil {
		if err := ExclusiveRename(tmpPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return false, nil
			}
			return false, fmt.Errorf("atomicfile: publish %s: %w", path, err)
		}
		return true, nil
	}
	if changed, err := changedSince(path, before); err != nil || changed {
		return false, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false, fmt.Errorf("atomicfile: rename %s -> %s: %w", tmpPath, path, err)
	}
	return true, nil
}
