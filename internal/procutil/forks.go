package procutil

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// HoldForks keeps this process from starting a child until release is
// called. A process that writes an executable and then runs it, or lets a
// command it starts run it, holds this while the file is open for writing:
// a child forked in that window keeps the descriptor until its own exec, and
// the kernel refuses to run a file that has a writer (ETXTBSY, "text file
// busy"). Every process start in this process waits for the release, so a
// process that only writes an executable for another process to run does
// not take it. It takes syscall.ForkLock for reading, so concurrent writers
// do not wait for each other.
//
// A write that can take long (copying a large file) uses WriteExecutable,
// which holds process starts off for less where the platform allows.
func HoldForks() (release func()) {
	syscall.ForkLock.RLock()
	return syscall.ForkLock.RUnlock
}

// InstallExecutable writes data to a new file beside path with mode perm and
// renames it over path: whatever is at path, a file or a link, is replaced
// and never written through. Process starts are held off (HoldForks) from
// the new file's creation until it is closed, so no child of this process
// can hold it open when it is run. No code of the caller runs while they are
// held off.
//
// WriteExecutable holds them off for less on Linux: it waits only for the
// starts in progress once the write is done, which is sound only while every
// start is a vfork whose child has exec'd before it releases ForkLock. A
// start that asks for a new user namespace (internal/netisolate) is not, so
// test code, whose process may make one, writes executables here; the app,
// which makes none, copies a large executable with WriteExecutable.
func InstallExecutable(path string, data []byte, perm fs.FileMode) error {
	return install(path, perm, func(file *os.File) error {
		_, err := file.Write(data)
		return err
	})
}

// CopyExecutable is InstallExecutable with the bytes of the regular file at
// src, streamed rather than held in memory. A non-nil digest receives the
// bytes in the same pass.
func CopyExecutable(path, src string, perm fs.FileMode, digest hash.Hash) (err error) {
	// A source that is not a regular file, such as a FIFO, could block the
	// copy while process starts are held off.
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("install %s: %s is not a regular file", path, src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := in.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", src, closeErr))
		}
	}()
	return install(path, perm, func(file *os.File) error {
		var out io.Writer = file
		if digest != nil {
			out = io.MultiWriter(file, digest)
		}
		_, err := io.Copy(out, in)
		return err
	})
}

// install fills a new file beside path with forks held off and renames it
// over path.
func install(path string, perm fs.FileMode, fill func(*os.File) error) error {
	temp, err := writeHeld(path, perm, fill)
	if err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return errors.Join(fmt.Errorf("install %s: %w", path, err), removeTemp(temp))
	}
	return nil
}

// writeHeld fills a new file beside path with forks held off and returns its
// name. The name is short and fixed apart from its random part, so any name
// path can have can be installed.
func writeHeld(path string, perm fs.FileMode, fill func(*os.File) error) (string, error) {
	release := HoldForks()
	defer release()
	file, err := os.CreateTemp(filepath.Dir(path), ".install-*")
	if err != nil {
		return "", fmt.Errorf("install %s: %w", path, err)
	}
	fillErr := fill(file)
	if err := errors.Join(fillErr, file.Chmod(perm), file.Close()); err != nil {
		return "", errors.Join(fmt.Errorf("install %s: %w", path, err), removeTemp(file.Name()))
	}
	return file.Name(), nil
}

func removeTemp(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}
