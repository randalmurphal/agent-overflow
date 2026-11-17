//go:build windows

package webview2host

import (
	"fmt"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A real-WebView2 smoke runs on a desktop of its own, so nothing it or
// WebView2 opens can reach the user's screen or keyboard. With
// AO_NATIVE_BROWSER_SMOKE=1 the test binary creates a desktop in the
// user's window station, which is never switched to, and runs itself again
// there (STARTUPINFO.lpDesktop); the WebView2 processes it starts inherit
// the desktop, and the child dies with the parent (a kill-on-close job).
// A binary that cannot make the desktop refuses to run the smokes, and a
// smoke refuses to open a window on any other desktop (requireSmokeDesktop),
// unless smokeUserDesktopEnv is 1.

const (
	// smokeDesktopEnv names the private desktop to the child that runs on it.
	smokeDesktopEnv = "AO_SMOKE_DESKTOP"
	// smokeUserDesktopEnv=1 runs the smokes in place, on the user's desktop,
	// where their windows show.
	smokeUserDesktopEnv = "AO_NATIVE_BROWSER_SMOKE_ON_USER_DESKTOP"
)

var (
	procCreateDesktop     = user32.NewProc("CreateDesktopW")
	procCloseDesktop      = user32.NewProc("CloseDesktop")
	procGetThreadDesktop  = user32.NewProc("GetThreadDesktop")
	procGetUserObjectInfo = user32.NewProc("GetUserObjectInformationW")
)

// uoiName is GetUserObjectInformation's UOI_NAME.
const uoiName = 2

func TestMain(m *testing.M) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") == "1" && os.Getenv(smokeDesktopEnv) == "" && os.Getenv(smokeUserDesktopEnv) != "1" {
		code, err := runOnPrivateDesktop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "the smokes refuse to run: they cannot run on a desktop of their own (%v); set %s=1 to run them on yours, where their windows show\n", err, smokeUserDesktopEnv)
			os.Exit(2)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// requireSmokeDesktop fails a smoke about to open a window anywhere but the
// desktop the runner made for it, unless smokeUserDesktopEnv allows it.
func requireSmokeDesktop(t *testing.T) {
	t.Helper()
	if os.Getenv(smokeUserDesktopEnv) == "1" {
		return
	}
	name, err := threadDesktopName()
	if err != nil {
		t.Fatalf("the smoke cannot tell which desktop it is on: %v", err)
	}
	if want := os.Getenv(smokeDesktopEnv); want == "" || name != want {
		t.Fatalf("the smoke would open a window on desktop %q, not its own %q; set %s=1 to allow it", name, want, smokeUserDesktopEnv)
	}
}

// runOnPrivateDesktop runs this binary, with its arguments, on a new
// desktop and answers its exit code.
func runOnPrivateDesktop() (int, error) {
	name := fmt.Sprintf("ao-smoke-%d", os.Getpid())
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	desktop, _, callErr := procCreateDesktop.Call(uintptr(unsafe.Pointer(namePtr)), 0, 0, 0, uintptr(windows.GENERIC_ALL), 0)
	if desktop == 0 {
		return 0, fmt.Errorf("CreateDesktop: %w", callErr)
	}
	defer func() { _, _, _ = procCloseDesktop.Call(desktop) }()

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	defer func() { _ = windows.CloseHandle(job) }()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return 0, err
	}
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(os.Args))
	if err != nil {
		return 0, err
	}
	startup := windows.StartupInfo{Desktop: namePtr, Flags: windows.STARTF_USESTDHANDLES}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	var std [3]windows.Handle
	for i, id := range []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE} {
		handle, err := windows.GetStdHandle(id)
		if err == nil && handle != 0 && handle != windows.InvalidHandle {
			// The child writes the test output to the parent's.
			_ = windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT)
			std[i] = handle
		}
	}
	startup.StdInput, startup.StdOutput, startup.StdErr = std[0], std[1], std[2]
	if err := os.Setenv(smokeDesktopEnv, name); err != nil {
		return 0, err
	}
	var process windows.ProcessInformation
	if err := windows.CreateProcess(exePtr, cmdline, nil, nil, true, windows.CREATE_SUSPENDED, nil, nil, &startup, &process); err != nil {
		return 0, fmt.Errorf("CreateProcess on desktop %s: %w", name, err)
	}
	defer func() { _ = windows.CloseHandle(process.Process) }()
	defer func() { _ = windows.CloseHandle(process.Thread) }()
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		_ = windows.TerminateProcess(process.Process, 1)
		return 0, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		_ = windows.TerminateProcess(process.Process, 1)
		return 0, fmt.Errorf("ResumeThread: %w", err)
	}
	if _, err := windows.WaitForSingleObject(process.Process, windows.INFINITE); err != nil {
		return 0, fmt.Errorf("wait for the smokes: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(process.Process, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}

// threadDesktopName answers the name of the calling thread's desktop.
func threadDesktopName() (string, error) {
	desktop, _, err := procGetThreadDesktop.Call(uintptr(windows.GetCurrentThreadId()))
	if desktop == 0 {
		return "", fmt.Errorf("GetThreadDesktop: %w", err)
	}
	var name [256]uint16
	var needed uint32
	ok, _, err := procGetUserObjectInfo.Call(desktop, uoiName, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)*2), uintptr(unsafe.Pointer(&needed)))
	if ok == 0 {
		return "", fmt.Errorf("GetUserObjectInformation: %w", err)
	}
	return windows.UTF16ToString(name[:]), nil
}

// The smokes run on the desktop the runner made for them, never the
// user's, and every thread of the binary is on it.
func TestSmokeRunsOnAPrivateDesktop(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" || os.Getenv(smokeUserDesktopEnv) == "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run the real-window checks on their own desktop")
	}
	name, err := threadDesktopName()
	if err != nil {
		t.Fatal(err)
	}
	if want := os.Getenv(smokeDesktopEnv); name != want || name == "Default" {
		t.Fatalf("the smoke runs on desktop %q, want its own %q", name, want)
	}
	ui := startSmokeUIThread(t)
	var uiDesktop string
	ui.do(func() { uiDesktop, err = threadDesktopName() })
	if err != nil || uiDesktop != name {
		t.Fatalf("the smoke window's thread is on desktop %q (%v), want %q", uiDesktop, err, name)
	}
}
