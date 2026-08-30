//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
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
func installHarnessBoundary(limit uint64) error {
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
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
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
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("SetInformationJobObject(memory): %w", err)
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(os.Getpid()),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("OpenProcess launcher: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("AssignProcessToJobObject launcher: %w", err)
	}
	// Keep the handle live until process exit. Closing it here would trigger
	// KILL_ON_JOB_CLOSE while the launcher is still serving the window.
	harnessJobHandle = job
	return nil
}

// The process owns this handle until Windows tears down the launcher. It is
// intentionally never closed during normal shutdown because this Job's
// KILL_ON_JOB_CLOSE is the final descendant backstop.
var harnessJobHandle windows.Handle
