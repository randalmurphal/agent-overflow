package git

import (
	"encoding/binary"
	"hash"
	"io/fs"
	"time"
)

// statRacyWindow is how long a file must have been left alone before its
// stat identifies its content: a same-size rewrite inside one timestamp
// tick of the filesystem leaves the stat as it was. Git's index has the
// same rule; 2s covers the coarsest common granularity (FAT, some network
// filesystems). A file inside the window is recomputed each scan.
const statRacyWindow = 2 * time.Second

// worktreeStat identifies a working tree file's content the way git's
// index does, by its stat. ctimeNs and inode are zero on a platform whose
// stat reports neither (statPath).
type worktreeStat struct {
	size    int64
	mtimeNs int64
	ctimeNs int64
	inode   uint64
	mode    fs.FileMode
}

// settledBefore reports whether the file was last written or replaced
// more than statRacyWindow before scanStart. The change time counts: a
// copy that preserves the modification time still moves it.
func (s worktreeStat) settledBefore(scanStart time.Time) bool {
	return time.Unix(0, max(s.mtimeNs, s.ctimeNs)).Add(statRacyWindow).Before(scanStart)
}

// writeTo writes the stat into h, in a form no absent file writes.
func (s worktreeStat) writeTo(h hash.Hash) {
	var buf [37]byte
	buf[0] = 1
	binary.LittleEndian.PutUint64(buf[1:], uint64(s.size))
	binary.LittleEndian.PutUint64(buf[9:], uint64(s.mtimeNs))
	binary.LittleEndian.PutUint64(buf[17:], uint64(s.ctimeNs))
	binary.LittleEndian.PutUint64(buf[25:], s.inode)
	binary.LittleEndian.PutUint32(buf[33:], uint32(s.mode))
	_, _ = h.Write(buf[:])
}

type worktreeFileState int

const (
	// worktreeFileUnsettled is a file that could not be read or is not
	// settledBefore the scan.
	worktreeFileUnsettled worktreeFileState = iota
	worktreeFileSettled
	worktreeFileMissing
)

// statWorktreeFile returns path's stat, following a link at path only
// when follow is set, and whether a scan begun at scanStart may key
// content by it.
func statWorktreeFile(path string, scanStart time.Time, follow bool) (worktreeStat, worktreeFileState) {
	stat, missing, err := statPath(path, follow)
	switch {
	case missing:
		return worktreeStat{}, worktreeFileMissing
	case err != nil || !stat.settledBefore(scanStart):
		return worktreeStat{}, worktreeFileUnsettled
	}
	return stat, worktreeFileSettled
}

// hashWorktreeFile writes path's stat, or its absence, into h, and
// returns which it found. An unsettled file writes nothing.
func hashWorktreeFile(h hash.Hash, path string, scanStart time.Time, follow bool) worktreeFileState {
	stat, state := statWorktreeFile(path, scanStart, follow)
	switch state {
	case worktreeFileSettled:
		stat.writeTo(h)
	case worktreeFileMissing:
		_, _ = h.Write([]byte{0})
	}
	return state
}
