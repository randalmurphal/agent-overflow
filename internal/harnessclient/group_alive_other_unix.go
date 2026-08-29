//go:build !windows && !linux

package harnessclient

// Non-Linux Unix hosts do not expose a stable procfs shape here. The signal-0
// result remains the conservative liveness answer on those platforms.
func processGroupHasLiveMember(int) bool { return true }
