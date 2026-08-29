//go:build windows

package containment

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsGroup struct {
	handle     windows.Handle
	configured bool
	limit      uint64
}

// Prepare creates a Job Object with a hard aggregate private-memory limit.
// KILL_ON_JOB_CLOSE is paired with an inherited job handle. A detached `up`
// command can close its own handle without killing the backend because the
// backend owns the inherited handle. When the backend exits, that last handle
// closes and the kernel kills any descendants left in the job.
func Prepare(limit uint64) (Group, error) {
	if limit == 0 {
		return nil, errors.New("harness containment: memory limit must be positive")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_JOB_MEMORY |
				windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
		JobMemoryLimit: uintptr(limit),
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject(memory): %w", err)
	}
	return &windowsGroup{handle: job, limit: limit}, nil
}

func (g *windowsGroup) Configure(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("harness containment: nil command")
	}
	if g == nil || g.handle == 0 {
		return errors.New("harness containment: job is closed")
	}
	if g.configured {
		return errors.New("harness containment: command already configured")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := windows.SetHandleInformation(g.handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return fmt.Errorf("SetHandleInformation(job inherit): %w", err)
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(g.handle))
	g.configured = true
	return nil
}

func (g *windowsGroup) Adopt(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("harness containment: cannot adopt nil process")
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess pid=%d: %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(g.handle, proc); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if err := resumeProcess(proc); err != nil {
		return err
	}
	return nil
}

func (g *windowsGroup) Close() error {
	if g == nil || g.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(g.handle)
	g.handle = 0
	if err != nil {
		return fmt.Errorf("CloseHandle(job): %w", err)
	}
	return nil
}

// Kill terminates the owned Job Object, including descendants whose root
// process has already exited. It never falls back to PID-based taskkill.
func (g *windowsGroup) Kill() error {
	if g == nil || g.handle == 0 {
		return errors.New("harness containment: job is closed")
	}
	if err := windows.TerminateJobObject(g.handle, 1); err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	return nil
}

func (g *windowsGroup) Wait(timeout time.Duration) error {
	if g == nil || g.handle == 0 {
		return errors.New("harness containment: job is closed")
	}
	deadline := time.Now().Add(timeout)
	for {
		var info jobBasicAccountingInformation
		var returned uint32
		if err := windows.QueryInformationJobObject(g.handle, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &returned); err != nil {
			return fmt.Errorf("QueryInformationJobObject: %w", err)
		}
		if info.ActiveProcesses == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("harness containment: job still has %d active processes", info.ActiveProcesses)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

type jobBasicAccountingInformation struct {
	TotalUserTime            int64
	TotalKernelTime          int64
	ThisPeriodTotalUserTime  int64
	TotalPageFaultCount      uint32
	TotalProcesses           uint32
	ActiveProcesses          uint32
	TotalTerminatedProcesses uint32
}

var (
	ntdll               = windows.NewLazySystemDLL("ntdll.dll")
	procNtResumeProcess = ntdll.NewProc("NtResumeProcess")
)

func resumeProcess(process windows.Handle) error {
	if err := procNtResumeProcess.Find(); err != nil {
		return fmt.Errorf("locate NtResumeProcess: %w", err)
	}
	status, _, _ := procNtResumeProcess.Call(uintptr(process))
	if status != 0 {
		return fmt.Errorf("NtResumeProcess returned NTSTATUS 0x%x", status)
	}
	return nil
}
