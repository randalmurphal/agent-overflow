package deviceclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const profileWriteTimeout = 5 * time.Second

// lockProfile provides OS ownership across processes. Session/key file locks
// cover short transactions only; a separate legacy-renewal lock serializes
// exchanges with hosts lacking recovery. Ownership ends on exit or crash;
// the lock file itself stays so all contenders keep opening the same inode.
func lockProfile(ctx context.Context, dir, name string) (func(), error) {
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
			return func() { _ = f.Close() }, nil
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
