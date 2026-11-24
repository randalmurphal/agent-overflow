package atomicfile

import (
	"errors"

	"golang.org/x/sys/unix"
)

func platformExclusiveRename(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}

// exclusiveRenameUnsupported reports renamex_np's answers for "not here":
// ENOTSUP or EOPNOTSUPP from a filesystem without RENAME_EXCL, ENOSYS from
// a system without the call.
func exclusiveRenameUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS)
}
