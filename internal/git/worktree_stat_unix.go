//go:build linux || darwin

package git

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// statPath stats path, following a link at path only when follow is set.
// missing reports a path that names nothing, including one under a parent
// that is no longer a directory. A status scan stats every changed file
// on every watcher edge, so this reads the kernel's stat without
// allocating.
func statPath(path string, follow bool) (stat worktreeStat, missing bool, err error) {
	var st unix.Stat_t
	for {
		if follow {
			err = unix.Stat(path, &st)
		} else {
			err = unix.Lstat(path, &st)
		}
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return worktreeStat{}, true, nil
	}
	if err != nil {
		return worktreeStat{}, false, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	return worktreeStat{
		size:    st.Size,
		mtimeNs: st.Mtim.Nano(),
		ctimeNs: st.Ctim.Nano(),
		inode:   st.Ino,
		mode:    unixFileMode(uint32(st.Mode)),
	}, false, nil
}

// unixFileMode maps a stat mode to the type and permission bits of an
// fs.FileMode.
func unixFileMode(raw uint32) fs.FileMode {
	mode := fs.FileMode(raw & 0o777)
	switch raw & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFLNK:
		mode |= fs.ModeSymlink
	case unix.S_IFDIR:
		mode |= fs.ModeDir
	default:
		mode |= fs.ModeIrregular
	}
	return mode
}
