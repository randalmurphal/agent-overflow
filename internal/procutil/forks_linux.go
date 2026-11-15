package procutil

import "syscall"

// WriteExecutable runs write, which creates a file and closes it, so that a
// process started afterwards can run that file without ETXTBSY. Process
// starts are not held off while write runs; it waits for the starts already
// in progress once write returns. Go's Linux fork holds syscall.ForkLock
// for writing until the child has exec'd (CLONE_VFORK, which Go uses unless
// the start asks for a new user namespace), and the exec closes
// every descriptor this process opened (they are close-on-exec), so a child
// forked while the file was open has dropped it once the lock can be taken,
// and a child forked after the close never had it.
func WriteExecutable(write func() error) error {
	err := write()
	syscall.ForkLock.RLock()
	syscall.ForkLock.RUnlock()
	return err
}
