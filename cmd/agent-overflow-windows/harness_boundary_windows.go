//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// installHarnessBoundary puts the Windows launcher in the one Job Object
// that owns its WebView2 descendants and the WSL bridge process. The Linux
// backend gets a separate inherited RLIMIT_DATA from wsllauncher. A Windows Job
// cannot account for WSL guest memory, so the launcher also runs an exact
// identity watchdog over that process tree.
//
// The boundary is installed before Wails creates WebView2. Harness profiles
// fail closed when this cannot be installed. Production launches return nil
// and retain their existing lifetime and memory behaviour.
func installHarnessBoundary(limit uint64) (err error) {
	if activeProfile == "" {
		return nil
	}
	if limit == 0 {
		return errors.New("harness boundary: memory limit must be positive")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
	}
	// The limits are on the heap, where they stay put, and kept alive past
	// the call, which receives them only as an integer.
	info := &windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_JOB_MEMORY |
				windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
				windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK,
		},
		JobMemoryLimit: uintptr(limit),
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(info)),
		uint32(unsafe.Sizeof(*info)),
	); err != nil {
		return closeJobAfter(job, fmt.Errorf("SetInformationJobObject(memory): %w", err))
	}
	runtime.KeepAlive(info)
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(os.Getpid()),
	)
	if err != nil {
		return closeJobAfter(job, fmt.Errorf("OpenProcess launcher: %w", err))
	}
	defer func() {
		if closeErr := windows.CloseHandle(process); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("CloseHandle launcher process: %w", closeErr))
		}
	}()
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return closeJobAfter(job, fmt.Errorf("AssignProcessToJobObject launcher: %w", err))
	}
	// Keep the handle live until process exit. Closing it here would trigger
	// KILL_ON_JOB_CLOSE while the launcher is still serving the window.
	harnessJobHandle = job
	return nil
}

// closeJobAfter closes a job the boundary could not install, joining a
// failed close into cause.
func closeJobAfter(job windows.Handle, cause error) error {
	if err := windows.CloseHandle(job); err != nil {
		return errors.Join(cause, fmt.Errorf("CloseHandle job: %w", err))
	}
	return cause
}

// The process owns this handle until Windows tears down the launcher. It is
// intentionally never closed during normal shutdown because this Job's
// KILL_ON_JOB_CLOSE is the final descendant backstop.
var harnessJobHandle windows.Handle
