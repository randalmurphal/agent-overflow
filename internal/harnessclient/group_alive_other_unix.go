//go:build !windows && !linux && !darwin

package harnessclient

// Other Unix hosts do not expose a stable procfs shape here. The signal-0
// result remains the conservative liveness answer on those platforms.
func processGroupHasLiveMember(int) bool { return true }
