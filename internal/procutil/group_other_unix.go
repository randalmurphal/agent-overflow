//go:build !windows && !darwin

package procutil

// groupExited is false here: Linux signals a member that has exited but is
// not yet reaped without error, so EPERM always names a member this process
// may not signal.
func groupExited(int) bool { return false }
