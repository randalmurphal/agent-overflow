//go:build windows

package procutil

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SetJobInformation sets job's information of class to info, a limit
// structure such as windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
// windows.SetInformationJobObject takes the buffer's address as a plain
// uintptr and is no direct syscall, so the compiler neither keeps the
// buffer where that address names nor sees it used; info is pinned across
// the call instead, which also keeps it off the stack.
func SetJobInformation[T any](job windows.Handle, class uint32, info *T) error {
	var pinner runtime.Pinner
	pinner.Pin(info)
	defer pinner.Unpin()
	_, err := windows.SetInformationJobObject(job, class, uintptr(unsafe.Pointer(info)), uint32(unsafe.Sizeof(*info)))
	return err
}

// QueryJobInformation reads job's information of class into info, pinned
// across the call as SetJobInformation's is.
func QueryJobInformation[T any](job windows.Handle, class int32, info *T) error {
	var pinner runtime.Pinner
	pinner.Pin(info)
	defer pinner.Unpin()
	var returned uint32
	return windows.QueryInformationJobObject(job, class, uintptr(unsafe.Pointer(info)), uint32(unsafe.Sizeof(*info)), &returned)
}
