// Package filelock holds exclusive advisory locks on files across
// processes: flock on Unix, LockFileEx on Windows. The kernel releases a
// lock when its holder exits or crashes, so a lock another process can take
// proves its last holder is gone.
//
// A lock belongs to the open file, not the process: a forked child's copy
// of the descriptor keeps a flock until its exec. Release unlocks before it
// closes for that reason.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// pollInterval is how often Acquire retries a held lock.
const pollInterval = 20 * time.Millisecond

// Open opens path for locking, creating it with mode 0600 when missing.
func Open(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

// Acquire opens path and waits until it holds the exclusive lock or ctx
// ends. The caller releases the returned file with Release.
func Acquire(ctx context.Context, path string) (*os.File, error) {
	f, err := Open(path)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		locked, err := TryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if locked {
			return f, nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Release unlocks f and closes it. A failed unlock leaves the close to
// release the lock; both failures are returned.
func Release(f *os.File) error {
	unlockErr := Unlock(f)
	closeErr := f.Close()
	if unlockErr != nil {
		unlockErr = fmt.Errorf("filelock: unlock %s: %w", f.Name(), unlockErr)
	}
	return errors.Join(unlockErr, closeErr)
}
