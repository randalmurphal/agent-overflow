//go:build !windows && !linux && !darwin

package procutil

// Exited is false here: this platform's process state is not read.
func Exited(int) bool { return false }
