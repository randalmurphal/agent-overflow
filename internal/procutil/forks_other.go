//go:build !linux

package procutil

// WriteExecutable runs write, which creates a file and closes it, so that a
// process started afterwards can run that file. Here a fork releases
// syscall.ForkLock before its child execs, so a child forked while the file
// was open could still hold it after the close: process starts are held off
// for the whole write (HoldForks).
func WriteExecutable(write func() error) error {
	release := HoldForks()
	defer release()
	return write()
}
