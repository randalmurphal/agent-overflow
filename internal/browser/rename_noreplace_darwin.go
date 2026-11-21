//go:build darwin

package browser

import "golang.org/x/sys/unix"

// renameNoReplace renames from to to unless something is at to
// (renamex_np RENAME_EXCL), answering EEXIST then.
func renameNoReplace(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
