//go:build unix

package harnessclient

import (
	"os"
	"strconv"
	"syscall"
)

// FileIdentity is the filesystem's own name for a file: device plus
// inode. A log that was rotated away and replaced gets a new inode,
// which is the only signal that survives the replacement growing back
// past the offset a reader stopped at — to a size comparison that looks
// like ordinary growth.
//
// It lives here rather than in the CLI because both readers of a rotated
// evidence file need it: `health`'s since-last-check cursor and
// FollowFile's `-f` loop.
//
// A Sys() that is not the expected type (an exotic FS driver) returns
// empty, which puts the caller back on the size heuristic alone rather
// than inventing an identity that would never match.
func FileIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return ""
	}
	return strconv.FormatUint(uint64(stat.Dev), 10) + ":" + strconv.FormatUint(uint64(stat.Ino), 10)
}
