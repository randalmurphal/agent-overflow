package atomicfile

import (
	"os"
	"path/filepath"
)

// RenameNoReplace publishes a file or directory on the SAME filesystem without
// replacing any existing destination, including an empty directory or symlink.
// Stat followed by os.Rename is not equivalent: another creator can win between
// them. A sync failure can mean the rename already happened; callers recover by
// their durable operation identity instead of retrying an unverified overwrite.
func RenameNoReplace(from, to string) error {
	if err := renameNoReplace(from, to); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	if err := SyncDir(filepath.Dir(to)); err != nil {
		return err
	}
	if filepath.Dir(from) != filepath.Dir(to) {
		return SyncDir(filepath.Dir(from))
	}
	return nil
}
