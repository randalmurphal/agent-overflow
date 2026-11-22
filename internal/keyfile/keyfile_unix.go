//go:build unix

package keyfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Every open goes through the folder's own descriptor (openat), opened and
// checked once: replacing the folder after its check cannot redirect the
// file open. O_NOFOLLOW refuses a symlink at each step without a separate
// check that could race the open; O_NONBLOCK keeps a FIFO planted at the
// path from blocking it.

// openOwnerOnly opens the key file for reading after checking that it and
// its folder are this user's own, putting their modes back to owner-only.
func (f File) openOwnerOnly() (file *os.File, err error) {
	folder, err := f.openFolder()
	if err != nil {
		return nil, err
	}
	defer func() {
		f.closeInto(&err, folder.Close, "folder")
		if err != nil && file != nil {
			f.closeInto(&err, file.Close, "file")
			file = nil
		}
	}()
	return f.openIn(folder)
}

// createOwnerOnly makes the key file, empty and 0600, in its existing
// folder. An existing file is checked as a read checks it and left alone.
func (f File) createOwnerOnly() (created bool, err error) {
	folder, err := f.openFolder()
	if err != nil {
		return false, err
	}
	defer f.closeInto(&err, folder.Close, "folder")
	fd, err := unix.Openat(int(folder.Fd()), filepath.Base(f.path), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(filePerm))
	if errors.Is(err, unix.EEXIST) {
		existing, err := f.openIn(folder)
		if err != nil {
			return false, err
		}
		return false, existing.Close()
	}
	if err != nil {
		return false, f.failed("create", "file", err)
	}
	file := os.NewFile(uintptr(fd), f.path)
	// The umask can only narrow the mode; set it exactly, on the descriptor.
	chmodErr := file.Chmod(filePerm)
	if err := errors.Join(chmodErr, file.Close()); err != nil {
		return false, f.failed("create", "file", err)
	}
	return true, nil
}

// removeOwnerOnly unlinks the key file's name in its checked folder.
// unlinkat never follows the name, so a symlink there is removed itself.
func (f File) removeOwnerOnly() (err error) {
	folder, err := f.openFolder()
	if errors.Is(err, ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.closeInto(&err, folder.Close, "folder")
	name := filepath.Base(f.path)
	var stat unix.Stat_t
	err = unix.Fstatat(int(folder.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil
	case err != nil:
		return f.failed("inspect", "file", err)
	case stat.Mode&unix.S_IFMT == unix.S_IFDIR:
		return f.refused("it is not a regular file")
	}
	if err := unix.Unlinkat(int(folder.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return f.failed("remove", "file", err)
	}
	return nil
}

// openFolder opens the key's folder without following a symlink and checks
// it is this user's own folder.
func (f File) openFolder() (*os.File, error) {
	dir := filepath.Dir(f.path)
	folder, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, f.label.ErrMissing()
		case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
			// Lstat only words the refusal; the open already refused.
			if info, statErr := os.Lstat(dir); statErr == nil && info.Mode()&fs.ModeSymlink != 0 {
				return nil, f.refused("its folder is a symlink")
			}
			return nil, f.refused("its folder is not a folder")
		}
		return nil, f.failed("open", "folder", err)
	}
	if err := f.checkOwned(folder, dirPerm, "its folder is not a folder", func(info fs.FileInfo) bool { return info.IsDir() }); err != nil {
		f.closeInto(&err, folder.Close, "folder")
		return nil, err
	}
	return folder, nil
}

// openIn opens the key file in its checked folder for reading and checks
// it.
func (f File) openIn(folder *os.File) (*os.File, error) {
	name := filepath.Base(f.path)
	fd, err := unix.Openat(int(folder.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, f.label.ErrMissing()
		case errors.Is(err, unix.ELOOP):
			return nil, f.refused("it is a symlink")
		}
		return nil, f.failed("open", "file", err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(folder.Name(), name))
	if err := f.checkOwned(file, filePerm, "it is not a regular file", func(info fs.FileInfo) bool { return info.Mode().IsRegular() }); err != nil {
		f.closeInto(&err, file.Close, "file")
		return nil, err
	}
	return file, nil
}

// checkOwned refuses an open file of the wrong kind or another user's, and
// sets its mode to perm, on the descriptor, when it differs.
func (f File) checkOwned(file *os.File, perm fs.FileMode, wrongKind string, rightKind func(fs.FileInfo) bool) error {
	info, err := file.Stat()
	if err != nil {
		return f.failed("inspect", "file", err)
	}
	if !rightKind(info) {
		return f.refused(wrongKind)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return f.refused("another user owns it")
	}
	if info.Mode().Perm() != perm {
		if err := file.Chmod(perm); err != nil {
			return f.failed("make", "file owner-only", err)
		}
	}
	return nil
}
