package atomicfile

import "golang.org/x/sys/unix"

func platformExclusiveRename(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}

// exclusiveRenameUnsupportedErrno is renameat2's answer on a filesystem
// without RENAME_NOREPLACE, such as NFS or 9p.
const exclusiveRenameUnsupportedErrno = unix.EINVAL
