package procutil

import (
	"errors"
	"fmt"
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

// InstallExecutable writes what src yields to a new file beside path with
// mode perm and renames it over path: whatever is at path, a file or a
// link, is replaced and never written through. Process starts are held off
// (HoldForks) from the new file's creation until it is closed, so no child
// of this process can hold it open when it is run.
//
// WriteExecutable holds them off for less on Linux: it waits only for the
// starts in progress once the write is done, which is sound only while every
// start is a vfork whose child has exec'd before it releases ForkLock. A
// start that asks for a new user namespace (internal/netisolate) is not, so
// test code, whose process may make one, writes executables here; the app,
// which makes none, copies a large executable with WriteExecutable.
func InstallExecutable(path string, src io.Reader, perm fs.FileMode) error {
	temp, err := writeHeld(path, src, perm)
	if err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return errors.Join(fmt.Errorf("install %s: %w", path, err), removeTemp(temp))
	}
	return nil
}

// writeHeld writes src to a new file beside path with forks held off and
// returns the file's name.
func writeHeld(path string, src io.Reader, perm fs.FileMode) (string, error) {
	release := HoldForks()
	defer release()
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".install-*")
	if err != nil {
		return "", fmt.Errorf("install %s: %w", path, err)
	}
	_, copyErr := io.Copy(file, src)
	if err := errors.Join(copyErr, file.Chmod(perm), file.Close()); err != nil {
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
