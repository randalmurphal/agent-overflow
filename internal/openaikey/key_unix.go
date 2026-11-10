//go:build unix

package openaikey

import (
	"errors"
	"fmt"
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
func openOwnerOnly(path string) (file *os.File, err error) {
	folder, err := openFolder(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		closeInto(&err, folder.Close, "folder")
		if err != nil && file != nil {
			closeInto(&err, file.Close, "file")
			file = nil
		}
	}()
	return openIn(folder, filepath.Base(path))
}

// createOwnerOnly makes the key file, empty and 0600, in its existing
// folder. An existing file is checked as a read checks it and left alone.
func createOwnerOnly(path string) (created bool, err error) {
	folder, err := openFolder(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer closeInto(&err, folder.Close, "folder")
	name := filepath.Base(path)
	fd, err := unix.Openat(int(folder.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(filePerm))
	if errors.Is(err, unix.EEXIST) {
		existing, err := openIn(folder, name)
		if err != nil {
			return false, err
		}
		return false, existing.Close()
	}
	if err != nil {
		return false, fmt.Errorf("create the OpenAI API key file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	// The umask can only narrow the mode; set it exactly, on the descriptor.
	chmodErr := file.Chmod(filePerm)
	if err := errors.Join(chmodErr, file.Close()); err != nil {
		return false, fmt.Errorf("create the OpenAI API key file: %w", err)
	}
	return true, nil
}

// openFolder opens the key's folder without following a symlink and checks
// it is this user's own folder.
func openFolder(dir string) (*os.File, error) {
	folder, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, ErrMissing
		case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
			// Lstat only words the refusal; the open already refused.
			if info, statErr := os.Lstat(dir); statErr == nil && info.Mode()&fs.ModeSymlink != 0 {
				return nil, refused("its folder is a symlink")
			}
			return nil, refused("its folder is not a folder")
		}
		return nil, fmt.Errorf("open the OpenAI API key folder: %w", err)
	}
	if err := checkOwned(folder, dirPerm, "its folder is not a folder", func(info fs.FileInfo) bool { return info.IsDir() }); err != nil {
		closeInto(&err, folder.Close, "folder")
		return nil, err
	}
	return folder, nil
}

// openIn opens name in the checked folder for reading and checks it.
func openIn(folder *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(folder.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, ErrMissing
		case errors.Is(err, unix.ELOOP):
			return nil, refused("it is a symlink")
		}
		return nil, fmt.Errorf("open the OpenAI API key file: %w", err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(folder.Name(), name))
	if err := checkOwned(file, filePerm, "it is not a regular file", func(info fs.FileInfo) bool { return info.Mode().IsRegular() }); err != nil {
		closeInto(&err, file.Close, "file")
		return nil, err
	}
	return file, nil
}

// checkOwned refuses an open file of the wrong kind or another user's, and
// sets its mode to perm, on the descriptor, when it differs.
func checkOwned(file *os.File, perm fs.FileMode, wrongKind string, rightKind func(fs.FileInfo) bool) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect the OpenAI API key file: %w", err)
	}
	if !rightKind(info) {
		return refused(wrongKind)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return refused("another user owns it")
	}
	if info.Mode().Perm() != perm {
		if err := file.Chmod(perm); err != nil {
			return fmt.Errorf("make the OpenAI API key file owner-only: %w", err)
		}
	}
	return nil
}
