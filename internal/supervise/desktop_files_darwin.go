//go:build darwin

package supervise

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// NativeDesktopFiles swaps a bundle into place with renamex_np(RENAME_SWAP)
// and keeps a bundle or executable lsof finds in use.
func NativeDesktopFiles() DesktopFiles {
	return DesktopFiles{
		Replace: func(staged, install, previous string) error {
			if !isBundle(install) {
				return os.Rename(staged, install)
			}
			return replaceBundle(staged, install, previous, func(from, to string) error {
				return unix.RenamexNp(from, to, unix.RENAME_SWAP)
			})
		},
		InUse: lsofInUse,
	}
}

// lsofInUseTimeout bounds one lsof walk of a bundle.
const lsofInUseTimeout = 30 * time.Second

// lsofInUse reports whether any process has path, or a file under a
// directory path, open, as scripts/macos-bundle.sh decides before it
// deletes a retired bundle. lsof exits 1 when nothing is open; anything it
// prints, a process or an error, keeps the path. +D takes only a
// directory: given a file, lsof prints its usage, so a file is named
// directly.
func lsofInUse(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	args := []string{"-nP", "-t", "--", path}
	if info.IsDir() {
		args = []string{"-nP", "-t", "+D", path}
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofInUseTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", args...).CombinedOutput()
	if len(bytes.TrimSpace(out)) > 0 {
		return true, nil
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return false, err
	}
	return false, nil
}
