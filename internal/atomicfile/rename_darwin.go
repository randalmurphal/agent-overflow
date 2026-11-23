package atomicfile

import "golang.org/x/sys/unix"

func platformExclusiveRename(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}

// exclusiveRenameUnsupportedErrno is renamex_np's answer on a filesystem
// without RENAME_EXCL.
const exclusiveRenameUnsupportedErrno = unix.ENOTSUP
