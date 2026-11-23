package atomicfile

import (
	"errors"

	"golang.org/x/sys/unix"
)

func platformExclusiveRename(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}

// exclusiveRenameUnsupported reports renameat2's answers for "not here":
// EINVAL from a filesystem without RENAME_NOREPLACE (NFS, 9p), EOPNOTSUPP
// from one that says so, ENOSYS from a kernel or sandbox without the call.
func exclusiveRenameUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS)
}
