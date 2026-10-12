package deviceclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// profileWriteTimeout bounds every wait for a profile lock. A variable so a
// test can shorten it; nothing else assigns it.
var profileWriteTimeout = 5 * time.Second

// lockProfile provides OS ownership across processes. Session/key file locks
// cover short transactions only; a separate legacy-renewal lock serializes
// exchanges with hosts lacking recovery. Ownership ends on exit or crash;
// the lock file itself stays so all contenders keep opening the same inode.
func lockProfile(ctx context.Context, dir, name string) (func() error, error) {
	if dir == "" || name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, errors.New("deviceclient: invalid profile lock location")
	}
	root := filepath.Join(dir, "locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		locked, err := tryProfileLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if locked {
			return func() error { return releaseProfile(f) }, nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// releaseProfile ends a lock's ownership. flock belongs to the open file
// description, so a forked child's copy keeps it until exec: unlock rather
// than rely on the close. A failed unlock leaves the close to release it;
// either failure is returned so the caller's operation reports it.
func releaseProfile(f *os.File) error {
	unlockErr := unlockProfile(f)
	closeErr := f.Close()
	if unlockErr != nil {
		unlockErr = fmt.Errorf("deviceclient: unlock %s: %w", f.Name(), unlockErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("deviceclient: close %s: %w", f.Name(), closeErr)
	}
	return errors.Join(unlockErr, closeErr)
}

// WithProfileLock serializes a short catalog transaction across controllers
// sharing an installation. Network work must finish before entering it.
func WithProfileLock(ctx context.Context, dir, name string, transaction func() error) (err error) {
	release, err := lockProfile(ctx, dir, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	return transaction()
}
