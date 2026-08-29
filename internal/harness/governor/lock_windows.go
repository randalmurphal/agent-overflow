//go:build windows

package governor

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type fileLock struct{ f *os.File }

func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("harness governor: open lock: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("harness governor: secure lock: %w", err)
	}
	// LockFileEx blocks until the first byte is exclusively held. The
	// kernel releases it if this process dies, unlike a marker-file lock.
	if err := windows.LockFileEx(windows.Handle(f.Fd()), 0, 0, 1, 0, &windows.Overlapped{}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("harness governor: acquire lock: %w", err)
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &windows.Overlapped{})
	if closeErr := l.f.Close(); err == nil {
		err = closeErr
	}
	return err
}
