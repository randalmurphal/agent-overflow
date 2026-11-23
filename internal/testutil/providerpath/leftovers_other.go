//go:build !linux

package providerpath

// markProcesses checks nothing off Linux: macOS has no /proc to find the
// processes a test left by their environment.
func markProcesses() (check func() error, err error) {
	return func() error { return nil }, nil
}
