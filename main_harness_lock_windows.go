//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFileExclusiveNonBlocking takes an exclusive LockFileEx byte-range
// lock over the whole file. Reports (false, nil) when another process
// holds it.
//
// Windows releases the range when the handle closes, including on an
// abnormal termination, so this has the same crash-recovery property as
// the unix flock: a killed boot leaves the next one free.
func lockFileExclusiveNonBlocking(file *os.File) (bool, error) {
	handle := windows.Handle(file.Fd())
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		// Lock the maximum range rather than the current length: the file
		// is rewritten by whoever holds it, and a length-derived range
		// would leave bytes past the old end unlocked.
		^uint32(0), ^uint32(0),
		&overlapped,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, err
}
