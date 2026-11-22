//go:build linux

package browser

import "golang.org/x/sys/unix"

// renameNoReplace renames from to to unless something is at to
// (renameat2 RENAME_NOREPLACE), answering EEXIST then.
func renameNoReplace(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
