//go:build unix

package app

import (
	"os"
	"syscall"
)

// providerBinaryFileID is the filesystem's name for a file. A file replaced
// at the same path gets a new one.
type providerBinaryFileID struct {
	device uint64
	inode  uint64
}

func binaryFileID(info os.FileInfo) providerBinaryFileID {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return providerBinaryFileID{}
	}
	return providerBinaryFileID{device: uint64(stat.Dev), inode: stat.Ino}
}
