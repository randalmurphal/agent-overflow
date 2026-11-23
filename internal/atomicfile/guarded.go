package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ReadWithStat reads path with the file's info as of the read, for a
// read-modify-write against another writer that may replace the file at
// any time (a provider CLI's own config). A missing file is (nil, nil,
// nil).
//
// The order is stat, read, stat: a writer that renames a new version in
// between would otherwise pair one version's bytes with the next
// version's info, which WriteIfUnchanged would then accept, clobbering
// that write. When the two stats are not the same version (sameVersion)
// the read is retried a bounded number of times; a writer on a sub-second
// cadence converges at once.
func ReadWithStat(path string) ([]byte, os.FileInfo, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		before, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("stat %s: %w", path, statErr)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("read %s: %w", path, readErr)
		}
		after, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("re-stat %s: %w", path, statErr)
		}
		if sameVersion(before, after) {
			return data, before, nil
		}
	}
	return nil, nil, fmt.Errorf("read %s: file changed during read across %d attempts", path, maxAttempts)
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
// cannot replace a file (RenameNoReplace's primitive), so that case has no
// such window. On Windows os.SameFile reads both identities from the path
// when compared, so the identity half of the check adds nothing there; the
// app runs on Windows through WSL, where this is Linux.
func WriteIfUnchanged(path string, data []byte, before os.FileInfo) (bool, error) {
	tmpPath, err := stage(path, data)
	if err != nil {
		return false, err
	}
	published, err := publishIfUnchanged(tmpPath, path, before)
	if err != nil || !published {
		_ = os.Remove(tmpPath)
		return false, err
	}
	if err := SyncDir(filepath.Dir(path)); err != nil {
		return true, err
	}
	return true, nil
}

// publishIfUnchanged renames the staged tmpPath onto path when path is
// still the version before names, and reports whether it did.
func publishIfUnchanged(tmpPath, path string, before os.FileInfo) (bool, error) {
	if before == nil {
		if err := renameNoReplace(tmpPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return false, nil
			}
			return false, fmt.Errorf("atomicfile: publish %s: %w", path, err)
		}
		return true, nil
	}
	current, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("atomicfile: recheck %s: %w", path, err)
	}
	if !sameVersion(before, current) {
		return false, nil
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false, fmt.Errorf("atomicfile: rename %s -> %s: %w", tmpPath, path, err)
	}
	return true, nil
}
