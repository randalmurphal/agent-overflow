package atomicfile

import (
	"errors"
	"fmt"
	"os"
)

// ReadWithStat reads path with the file's info as of the read, for a
// read-modify-write against another writer that may replace the file at
// any time (a provider CLI's own config). A missing file is (nil, nil,
// nil).
//
// The order is stat, read, stat: a writer that renames a new version in
// between would otherwise pair one version's bytes with the next
// version's info, which WriteIfUnchanged would then accept, clobbering
// that write. When the two stats differ the read is retried a bounded
// number of times; a writer on a sub-second cadence converges at once.
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
		if before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) {
			return data, before, nil
		}
	}
	return nil, nil, fmt.Errorf("read %s: file changed during read across %d attempts", path, maxAttempts)
}

// WriteIfUnchanged replaces path with data, as Write does, only while the
// file's size and modification time still match before, the info
// ReadWithStat answered (nil for a file that was absent). It answers
// false with no error when another writer changed the file since, so the
// caller can read again and retry.
func WriteIfUnchanged(path string, data []byte, before os.FileInfo) (bool, error) {
	if before != nil {
		current, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("recheck %s: %w", path, err)
		}
		if err == nil && (current.Size() != before.Size() || !current.ModTime().Equal(before.ModTime())) {
			return false, nil
		}
	}
	if err := Write(path, data); err != nil {
		return false, err
	}
	return true, nil
}
