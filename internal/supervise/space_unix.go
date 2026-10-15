//go:build !windows

package supervise

import "golang.org/x/sys/unix"

// ReadDiskSpace reads the filesystem that holds path.
func ReadDiskSpace(path string) (DiskSpace, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return DiskSpace{}, err
	}
	size := uint64(stat.Bsize)
	return DiskSpace{Available: uint64(stat.Bavail) * size, Used: (stat.Blocks - stat.Bfree) * size}, nil
}
