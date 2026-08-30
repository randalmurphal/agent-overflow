//go:build windows

package harnessrun

// Windows does not expose a portable, non-signalling process probe through
// os.Process. Fail closed so an orphaned lock is retried only after an
// operator removes it, never by guessing that a PID is gone.
func lockProcessAlive(pid int) bool { return pid > 0 }
