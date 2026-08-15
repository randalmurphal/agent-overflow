//go:build windows

package goroutinedump

// Install is a no-op on Windows: there is no SIGUSR1, and the backend
// this dump exists to debug runs inside WSL anyway (the Windows binary
// is the launcher, cmd/agent-overflow-windows). Write remains available
// so a future Windows-side trigger only has to call it.
func Install(dir string, logf func(format string, args ...any)) (stop func()) {
	return func() {}
}
