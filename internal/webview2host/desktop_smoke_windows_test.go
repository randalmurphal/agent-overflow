//go:build windows

package webview2host

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"agent-overflow/internal/procutil"
)

// A real-WebView2 smoke runs on a desktop of its own, so nothing it or
// WebView2 opens can reach the user's screen or keyboard. With
// AO_NATIVE_BROWSER_SMOKE=1 the test binary creates a desktop in the
// user's window station, which is never switched to, and runs itself again
// there (STARTUPINFO.lpDesktop); the WebView2 processes it starts inherit
// the desktop, and the child dies with the parent (a kill-on-close job).
// A binary that cannot make the desktop refuses to run the smokes, and a
// smoke refuses to open a window on the interactive desktop
// (refuseInteractiveDesktop), decided from the creating thread's own
// desktop whatever the environment says, unless smokeUserDesktopEnv is 1.

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
	procOpenInputDesktop  = user32.NewProc("OpenInputDesktop")
	procOpenDesktop       = user32.NewProc("OpenDesktopW")
	procSetThreadDesktop  = user32.NewProc("SetThreadDesktop")
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

// requireSmokeDesktop fails a smoke whose thread is on the interactive
// desktop (refuseInteractiveDesktop).
func requireSmokeDesktop(t *testing.T) {
	t.Helper()
	if err := refuseInteractiveDesktop(); err != nil {
		t.Fatal(err)
	}
}

// refuseInteractiveDesktop answers an error when the calling thread, about
// to create a window, is on the interactive desktop: the window station's
// input desktop, or Default, which is the user's whenever the input
// desktop is the lock screen's. It reads the thread's actual desktop, not
// the runner's marker, and refuses when it cannot tell. Only
// smokeUserDesktopEnv=1 allows it.
func refuseInteractiveDesktop() error {
	if os.Getenv(smokeUserDesktopEnv) == "1" {
		return nil
	}
	name, err := threadDesktopName()
	if err != nil {
		return fmt.Errorf("the smoke cannot tell which desktop it is on: %w", err)
	}
	input, err := inputDesktopName()
	if err != nil {
		return fmt.Errorf("the smoke cannot tell which desktop is the user's: %w", err)
	}
	if name == input || strings.EqualFold(name, "Default") {
		return fmt.Errorf("the smoke would open a window on desktop %q, the user's (input desktop %q); set %s=1 to allow it", name, input, smokeUserDesktopEnv)
	}
	return nil
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
	limits := new(windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION)
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if err := procutil.SetJobInformation(job, windows.JobObjectExtendedLimitInformation, limits); err != nil {
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
	return desktopName(desktop)
}

// inputDesktopName answers the name of the desktop that receives the
// user's input.
func inputDesktopName() (string, error) {
	const desktopReadObjects = 0x0001
	desktop, _, err := procOpenInputDesktop.Call(0, 0, desktopReadObjects)
	if desktop == 0 {
		return "", fmt.Errorf("OpenInputDesktop: %w", err)
	}
	defer func() { _, _, _ = procCloseDesktop.Call(desktop) }()
	return desktopName(desktop)
}

// desktopName answers a desktop handle's name.
func desktopName(desktop uintptr) (string, error) {
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

// A thread on the user's desktop is refused a window, whatever the
// runner's marker names: checked on a thread of the binary's own moved to
// Default, which only asks and opens nothing.
func TestASmokeRefusesTheUsersDesktopWhateverTheMarker(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" || os.Getenv(smokeUserDesktopEnv) == "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run the real-window checks on their own desktop")
	}
	t.Setenv(smokeDesktopEnv, "Default")
	refused := make(chan error, 1)
	go func() {
		// Never unlocked: the thread ends with this goroutine, on Default.
		runtime.LockOSThread()
		refused <- func() error {
			name, err := windows.UTF16PtrFromString("Default")
			if err != nil {
				return err
			}
			const desktopReadObjects = 0x0001
			desktop, _, callErr := procOpenDesktop.Call(uintptr(unsafe.Pointer(name)), 0, 0, desktopReadObjects)
			if desktop == 0 {
				return fmt.Errorf("OpenDesktop: %w", callErr)
			}
			defer func() { _, _, _ = procCloseDesktop.Call(desktop) }()
			if ok, _, callErr := procSetThreadDesktop.Call(desktop); ok == 0 {
				return fmt.Errorf("SetThreadDesktop: %w", callErr)
			}
			err = refuseInteractiveDesktop()
			if err == nil {
				return errors.New("a thread on Default was not refused")
			}
			t.Logf("refused: %v", err)
			return nil
		}()
	}()
	if err := <-refused; err != nil {
		t.Fatal(err)
	}
	if err := refuseInteractiveDesktop(); err != nil {
		t.Fatalf("the smoke's own desktop was refused: %v", err)
	}
}
