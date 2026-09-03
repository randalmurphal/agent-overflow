//go:build !windows

package atomicfile

import "os"

// syncDir flushes the directory entry itself. Opening a directory read-only
// and calling fsync on it is the POSIX way to make a rename durable; the
// handle is closed either way, and a close error on a read-only descriptor
// carries nothing the sync did not already report.
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
