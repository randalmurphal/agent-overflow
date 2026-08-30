//go:build windows

package main

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type harnessFileAttributeTagInfo struct {
	Attributes uint32
	ReparseTag uint32
}

func openHarnessLock(path string, mode os.FileMode) (*os.File, error) {
	_ = mode
	name, err := windows.UTF16FromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		&name[0], windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var info harnessFileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("harness instance lock is a reparse point")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create lock file descriptor")
	}
	return file, nil
}

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
