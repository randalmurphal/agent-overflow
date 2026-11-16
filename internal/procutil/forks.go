package procutil

import "syscall"

// HoldForks keeps this process from starting a child until release is
// called. A process that writes an executable and then runs it, or lets a
// command it starts run it, holds this while the file is open for writing:
// a child forked in that window keeps the descriptor until its own exec, and
// the kernel refuses to run a file that has a writer (ETXTBSY, "text file
// busy"). Every process start in this process waits for the release, so a
// process that only writes an executable for another process to run does
// not take it. It takes syscall.ForkLock for reading, so concurrent writers
// do not wait for each other.
//
// A write that can take long (copying a large file) uses WriteExecutable,
// which holds process starts off for less where the platform allows.
func HoldForks() (release func()) {
	syscall.ForkLock.RLock()
	return syscall.ForkLock.RUnlock
}
