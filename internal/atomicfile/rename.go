package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrNoExclusiveMove reports a filesystem that cannot move without risking
// a replacement: it refuses the rename that cannot replace, and for a file
// it refuses hard links too. A directory is never linked, so for one the
// refused rename alone is this error. Callers ask with errors.Is.
var ErrNoExclusiveMove = errors.New("the filesystem cannot move a file without risking replacing another")

// fileOps are the filesystem operations of the exclusive move and the
// guarded read and write. The exported functions use the system's
// (systemOps); tests pass fakes to the unexported functions they call.
type fileOps struct {
	exclusiveRename func(from, to string) error
	rename          func(from, to string) error
	link            func(from, to string) error
	remove          func(name string) error
	lstat           func(name string) (os.FileInfo, error)
	stat            func(name string) (os.FileInfo, error)
	open            func(name string) (*os.File, error)
	stage           func(path string, data []byte) (string, error)
	syncDir         func(dir string) error
}

func systemOps() fileOps {
	return fileOps{
		exclusiveRename: platformExclusiveRename, rename: os.Rename, link: os.Link, remove: os.Remove,
		lstat: os.Lstat, stat: os.Stat, open: os.Open, stage: stage, syncDir: SyncDir,
	}
}

// RenameNoReplace publishes a file or directory on the SAME filesystem without
// replacing any existing destination, including an empty directory or symlink.
// Stat followed by os.Rename is not equivalent: another creator can win between
// them. It is ExclusiveRename followed by a sync of both parent directories,
// and answers the same leftover. A sync failure is returned after the move
// happened; callers recover by their durable operation identity instead of
// retrying an unverified overwrite.
func RenameNoReplace(from, to string) (leftover string, err error) {
	return renameNoReplace(systemOps(), from, to)
}

func renameNoReplace(ops fileOps, from, to string) (string, error) {
	leftover, err := exclusiveMove(ops, from, to)
	if err != nil {
		return "", &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	if err := ops.syncDir(filepath.Dir(to)); err != nil {
		return leftover, err
	}
	if filepath.Dir(from) != filepath.Dir(to) {
		return leftover, ops.syncDir(filepath.Dir(from))
	}
	return leftover, nil
}

// ExclusiveRename moves from to to on the same filesystem unless something
// is at to. A non-nil error means nothing was published: os.ErrExist when
// something is at to, ErrNoExclusiveMove when the filesystem offers no way
// to move without risking a replacement, or the failure itself. It does
// not sync either directory, so a crash can undo the move; a caller that
// needs the move to be durable uses RenameNoReplace.
//
// Some filesystems, such as NFS and 9p, refuse the exclusive rename. There
// a file is published by a hard link from to to, which also fails when
// something is at to, and from's name is then removed. If that removal
// fails, the file is published and from still names it too: leftover is
// from, for the caller to remove or report. It is empty otherwise. A crash
// between the link and the removal leaves the same two names.
func ExclusiveRename(from, to string) (leftover string, err error) {
	return exclusiveMove(systemOps(), from, to)
}

func exclusiveMove(ops fileOps, from, to string) (string, error) {
	renameErr := ops.exclusiveRename(from, to)
	if renameErr == nil {
		return "", nil
	}
	if !exclusiveRenameUnsupported(renameErr) {
		return "", renameErr
	}
	source, err := ops.lstat(from)
	if err != nil {
		return "", err
	}
	if source.IsDir() {
		return "", fmt.Errorf("%w: moving the directory %s to %s needs a rename that cannot replace, which this filesystem refuses (%v)", ErrNoExclusiveMove, from, to, renameErr)
	}
	if err := ops.link(from, to); err != nil {
		if LinkRefused(err) {
			return "", fmt.Errorf("%w: it refuses both a rename that cannot replace (%v) and a hard link (%v)", ErrNoExclusiveMove, renameErr, err)
		}
		return "", err
	}
	if err := ops.remove(from); err != nil {
		return from, nil
	}
	return "", nil
}

// LinkRefused reports whether a hard link failed because the filesystem
// does not make hard links, rather than for a reason the user can act on
// (no space, no permission, a missing source, an existing destination).
func LinkRefused(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}
