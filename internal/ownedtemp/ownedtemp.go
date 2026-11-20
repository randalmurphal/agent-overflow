// Package ownedtemp makes temporary directories that outlive no process
// for long. A directory holds a lock file its process keeps locked until
// it releases the directory or exits, however it exits; Sweep removes the
// directories nobody owns any more. A process that is killed, panics at
// a test timeout or calls os.Exit before its cleanup leaves a directory
// the next Sweep of that prefix removes. A Shared directory's lock is also
// held by the processes it is passed to, so it outlives none of them, and
// on Linux a directory can also stay owned while a mount namespace has a
// process (KeepForMountNamespace).
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

// ErrInUse is wrapped by Shared.Release when another process still owns
// the directory: it holds the lock, or is in the mount namespace the
// directory records.
var ErrInUse = errors.New("another process still owns it")

// Shared is a directory Make would create that processes this one starts
// use too.
type Shared struct {
	Dir string
	// Lock is the open lock file. Pass it to every process that uses Dir
	// (exec.Cmd.ExtraFiles) and keep it open across their exec. A flock
	// belongs to the open file all those copies share, so Dir stays owned,
	// and no Sweep removes it, until the last of them has exited, this
	// process included.
	Lock *os.File
}

// MakeShared creates a directory like Make, for processes this one starts
// to use and hold the lock of.
func MakeShared(parent, prefix string) (*Shared, error) {
	dir, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return nil, err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return &Shared{Dir: dir, Lock: lock}, nil
}

// Release closes this process's copy of the lock and removes Dir once no
// other process owns it. It never unlocks, which would release the lock
// for every process sharing it. When one still owns it, Dir stays for the
// Sweep after that process exits, and the error wraps ErrInUse.
func (s *Shared) Release() error {
	if err := s.Lock.Close(); err != nil {
		return fmt.Errorf("close the lock of %s: %w", s.Dir, err)
	}
	removed, err := removeIfOwnerGone(s.Dir)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("leave %s for a later sweep: %w", s.Dir, ErrInUse)
	}
	return nil
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
// prefix plus anything whose owner is gone: their lock file is there,
// nobody holds it, and no process is in the mount namespace a directory
// records (KeepForMountNamespace). A directory without the lock file stays: Make has not
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
		if _, err := removeIfOwnerGone(dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Held removes the directories Sweep would and counts the rest that a
// process owns, so a directory can stand for a claim that lasts as long as
// its owners. One without the lock file is not counted. Callers that
// count and then claim serialize both under a lock of their own, or two
// can count the same total. An error means the count is uncertain.
func Held(parent, prefix string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(parent, prefix+"*"))
	if err != nil {
		return 0, err
	}
	held := 0
	var errs []error
	for _, dir := range matches {
		removed, err := removeIfOwnerGone(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if removed {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, lockName)); err == nil {
			held++
		} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			errs = append(errs, fmt.Errorf("inspect %s: %w", dir, err))
		}
	}
	return held, errors.Join(errs...)
}

// removeIfOwnerGone removes dir when its lock file is there and nobody
// holds it, and reports whether it did.
func removeIfOwnerGone(dir string) (bool, error) {
	gone, err := ownerGone(dir)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", dir, err)
	}
	if !gone {
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("remove %s: %w", dir, err)
	}
	return true, nil
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
	inUse, err := mountNamespaceInUse(dir)
	return err == nil && !inUse, errors.Join(err, filelock.Release(file))
}
