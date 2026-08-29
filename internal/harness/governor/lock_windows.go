//go:build windows

package governor

import "fmt"

// Windows is deliberately refusal-only until the launcher can provide a
// tested LockFileEx implementation. Pretending an ordinary file is a mutex
// permits two worktrees to oversubscribe the host, which is worse than a
// loud refusal.
type fileLock struct{}

func acquireLock(string) (*fileLock, error) {
	return nil, fmt.Errorf("harness governor: %w (Windows locking is not implemented)", ErrUnsupported)
}
func (*fileLock) Close() error { return nil }
