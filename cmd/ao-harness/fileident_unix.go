//go:build unix

package main

import (
	"os"
	"strconv"
	"syscall"
)

// fileIdentity is the filesystem's own name for a file: device plus
// inode. A log that was rotated away and replaced gets a new inode, which
// is the only signal that survives the replacement growing back past the
// offset the last check stopped at.
//
// A Sys() that is not the expected type (an exotic FS driver) returns
// empty, which puts the cursor back on the size heuristic alone rather
// than inventing an identity that would never match.
func fileIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return ""
	}
	return strconv.FormatUint(uint64(stat.Dev), 10) + ":" + strconv.FormatUint(uint64(stat.Ino), 10)
}
