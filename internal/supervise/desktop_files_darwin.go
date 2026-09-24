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
// and keeps a bundle lsof finds in use.
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

// lsofInUse reports whether any process has a file under path open, as
// scripts/macos-bundle.sh decides before it deletes a retired bundle.
// lsof exits 1 when nothing is open; anything it prints, a process or an
// error, keeps the bundle.
func lsofInUse(path string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsofInUseTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-t", "+D", path).CombinedOutput()
	if len(bytes.TrimSpace(out)) > 0 {
		return true, nil
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return false, err
	}
	return false, nil
}
