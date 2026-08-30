//go:build !linux && !darwin && !windows

package governor

import "fmt"

// Platforms without a tested advisory-lock primitive refuse rather than
// silently allowing concurrent harnesses to overcommit the host.
type fileLock struct{}

func acquireLock(string) (*fileLock, error) {
	return nil, fmt.Errorf("harness governor: %w (OS locking is not implemented)", ErrUnsupported)
}

func (*fileLock) Close() error { return nil }
