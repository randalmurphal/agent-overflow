package deviceclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/filelock"
)

// profileWriteTimeout bounds every wait for a profile lock. A variable so a
// test can shorten it; nothing else assigns it.
var profileWriteTimeout = 5 * time.Second

// releaseProfileLock is what a lock's release calls. A variable so a test
// can make a release fail and see each caller report it; nothing else
// assigns it.
var releaseProfileLock = releaseProfile

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
	f, err := filelock.Acquire(ctx, filepath.Join(root, name+".lock"))
	if err != nil {
		return nil, err
	}
	return func() error { return releaseProfileLock(f) }, nil
}

// releaseProfile ends a lock's ownership; see filelock.Release.
func releaseProfile(f *os.File) error {
	return filelock.Release(f)
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
