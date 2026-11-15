//go:build !linux && !darwin

package git

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// statPath stats path, following a link at path only when follow is set.
// missing reports a path that names nothing, including one under a parent
// that is no longer a directory (ENOTDIR; Windows reports that path as
// not found). This platform's stat reports no change time or inode.
func statPath(path string, follow bool) (stat worktreeStat, missing bool, err error) {
	read := os.Lstat
	if follow {
		read = os.Stat
	}
	info, err := read(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return worktreeStat{}, true, nil
	}
	if err != nil {
		return worktreeStat{}, false, err
	}
	return worktreeStat{
		size:    info.Size(),
		mtimeNs: info.ModTime().UnixNano(),
		mode:    info.Mode() & (fs.ModeType | fs.ModePerm),
	}, false, nil
}
