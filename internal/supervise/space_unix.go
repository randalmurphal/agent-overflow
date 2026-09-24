//go:build !windows

package supervise

import "golang.org/x/sys/unix"

// FreeBytes is the space an unprivileged process can still write on the
// filesystem that holds path.
func FreeBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
