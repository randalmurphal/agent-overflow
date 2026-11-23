package atomicfile

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// RenameNoReplace publishes a file or directory on the SAME filesystem without
// replacing any existing destination, including an empty directory or symlink.
// Stat followed by os.Rename is not equivalent: another creator can win between
// them. A sync failure can mean the rename already happened; callers recover by
// their durable operation identity instead of retrying an unverified overwrite.
// On a filesystem without an exclusive rename only a file can be published
// (renameNoReplace).
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

// exclusiveRename and removeStagedName are the platform's exclusive rename
// and os.Remove; a test replaces them to stand in for a filesystem without
// the rename, or a staged name that cannot be removed.
var (
	exclusiveRename  = platformExclusiveRename
	removeStagedName = os.Remove
)

// renameNoReplace moves from to to unless something is at to, which it
// answers with an error matching os.ErrExist.
//
// Some filesystems, such as NFS and 9p, refuse the exclusive rename. There
// a file is published by a hard link from to to, which also fails when
// something is at to, and from's name is then removed. The link is not one
// step: a crash between the link and the removal leaves the file under
// both names. A failed removal is logged, not returned, since the file is
// published. A directory cannot be hard-linked, so on such a filesystem
// publishing one fails.
func renameNoReplace(from, to string) error {
	renameErr := exclusiveRename(from, to)
	if renameErr == nil || !exclusiveRenameUnsupported(renameErr) {
		return renameErr
	}
	if err := os.Link(from, to); err != nil {
		if errors.Is(err, os.ErrExist) {
			return err
		}
		return fmt.Errorf("the filesystem refuses a rename that cannot replace (%v), and a hard link, the fallback for a file, failed: %w", renameErr, err)
	}
	if err := removeStagedName(from); err != nil {
		log.Printf("atomicfile: published %s by a hard link, but its staged name %s remains: %v", to, from, err)
	}
	return nil
}
