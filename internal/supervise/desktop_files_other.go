//go:build !darwin && !windows

package supervise

import "os"

// NativeDesktopFiles replaces the installed binary with one rename. A Linux
// process keeps running from an executable that was replaced or removed, so
// nothing is ever kept for being in use.
func NativeDesktopFiles() DesktopFiles {
	return DesktopFiles{
		Replace: func(staged, install, _ string) error { return os.Rename(staged, install) },
		InUse:   func(string) (bool, error) { return false, nil },
	}
}
