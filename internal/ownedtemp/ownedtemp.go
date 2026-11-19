// Package ownedtemp makes temporary directories that outlive no process
// for long. A directory holds a lock file its process keeps locked until
// it releases the directory or exits, however it exits; Sweep removes the
// directories whose lock nobody holds. A process that is killed, panics at
// a test timeout or calls os.Exit before its cleanup leaves a directory
// the next Sweep of that prefix removes.
//
// Standard library and filelock only, so test fixtures that must stay
// light can use it.
package ownedtemp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"agent-overflow/internal/filelock"
)

// lockName is the file in each directory its owner keeps locked.
const lockName = ".ownedtemp-lock"

// Make creates a directory in parent (os.TempDir() when empty) named
// prefix plus a random suffix, owned by this process. release removes it;
// until then, or until the process exits, Sweep leaves it alone.
func Make(parent, prefix string) (dir string, release func() error, err error) {
	dir, err = os.MkdirTemp(parent, prefix)
	if err != nil {
		return "", nil, err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return "", nil, errors.Join(err, os.RemoveAll(dir))
	}
	return dir, func() error {
		// The lock goes first: Windows cannot remove an open file.
		releaseErr := filelock.Release(lock)
		removeErr := os.RemoveAll(dir)
		if removeErr != nil {
			removeErr = fmt.Errorf("remove %s: %w", dir, removeErr)
		}
		return errors.Join(releaseErr, removeErr)
	}, nil
}

// lockDir takes the lock that marks dir owned. The lock file appears under
// its final name only once locked, so a Sweep never takes it first.
func lockDir(dir string) (*os.File, error) {
	pending := filepath.Join(dir, lockName+".pending")
	file, err := filelock.Open(pending)
	if err != nil {
		return nil, fmt.Errorf("create the lock of %s: %w", dir, err)
	}
	locked, err := filelock.TryLock(file)
	if err == nil && !locked {
		err = errors.New("held by another process")
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("lock %s: %w", dir, err), file.Close())
	}
	if err := os.Rename(pending, filepath.Join(dir, lockName)); err != nil {
		return nil, errors.Join(fmt.Errorf("publish the lock of %s: %w", dir, err), filelock.Release(file))
	}
	return file, nil
}

// Sweep removes the directories in parent (os.TempDir() when empty) named
// prefix plus anything whose owner is gone: their lock file is there and
// nobody holds it. A directory without the lock file stays: Make has not
// locked it yet, or a process that does not use this package made it and
// may still use it. It returns what it could not inspect or remove.
func Sweep(parent, prefix string) error {
	if parent == "" {
		parent = os.TempDir()
	}
	matches, err := filepath.Glob(filepath.Join(parent, prefix+"*"))
	if err != nil {
		return err
	}
	var errs []error
	for _, dir := range matches {
		gone, err := ownerGone(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect %s: %w", dir, err))
			continue
		}
		if !gone {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
}

func ownerGone(dir string) (bool, error) {
	file, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	locked, err := filelock.TryLock(file)
	if err != nil || !locked {
		return false, errors.Join(err, file.Close())
	}
	return true, filelock.Release(file)
}
