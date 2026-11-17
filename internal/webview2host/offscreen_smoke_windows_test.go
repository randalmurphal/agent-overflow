//go:build windows

package webview2host

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A real-WebView2 smoke must never show anything on the user's screen or
// take their keyboard. watchSmokeWindows samples, for the whole smoke, every
// visible top-level window of this process and of the processes it started
// (the WebView2 browser and its helpers, which own popups, the download
// dialog and DevTools), and fails the smoke for any window that intersects a
// monitor or is the foreground window. Every smoke starts it through
// startSmokeUIThread.

var (
	procGetWindowRect  = user32.NewProc("GetWindowRect")
	procMonitorFromRec = user32.NewProc("MonitorFromRect")
	procLockForeground = user32.NewProc("LockSetForegroundWindow")
)

// smokeWatchInterval is how often the watcher samples. A window that flashes
// on screen for less than this can be missed; WebView2's own windows (a
// dialog, a popup) stay up far longer.
const smokeWatchInterval = 25 * time.Millisecond

const (
	monitorDefaultToNull = 0
	lsfwLock             = 1
	lsfwUnlock           = 2
)

func watchSmokeWindows(t *testing.T) {
	t.Helper()
	locked, _, lockErr := procLockForeground.Call(lsfwLock)
	t.Logf("LockSetForegroundWindow: %d %v", locked, lockErr)
	if locked != 0 {
		t.Cleanup(func() { _, _, _ = procLockForeground.Call(lsfwUnlock) })
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	seen := map[string]bool{}
	var problems []string
	start := time.Now()
	note := func(problem string) {
		mu.Lock()
		defer mu.Unlock()
		if !seen[problem] {
			seen[problem] = true
			problems = append(problems, fmt.Sprintf("%s, first seen %v into the smoke", problem, time.Since(start).Round(time.Millisecond)))
		}
	}
	samples := 0
	go func() {
		defer close(done)
		ticker := time.NewTicker(smokeWatchInterval)
		defer ticker.Stop()
		for {
			for _, problem := range smokeWindowProblems() {
				note(problem)
			}
			samples++
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		for _, problem := range smokeWindowProblems() {
			note(problem)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, problem := range problems {
			t.Errorf("a smoke window was visible to the user: %s", problem)
		}
		t.Logf("window watch: %d samples, %d problems", samples, len(problems))
	})
}

// smokeWindow is a top-level window of the smoke's process tree.
type smokeWindow struct {
	hwnd   uintptr
	pid    uint32
	bounds rect
}

// smokeWindowProblems answers every visible top-level window of the smoke's
// process tree that is on a monitor, and any of its windows that is the
// foreground window.
func smokeWindowProblems() []string {
	foreground, _, _ := procGetForeground.Call()
	var problems []string
	for _, w := range smokeWindows(foreground) {
		if w.hwnd == foreground {
			problems = append(problems, fmt.Sprintf("window %#x of process %d is the foreground window", w.hwnd, w.pid))
		}
		if windows.IsWindowVisible(windows.HWND(w.hwnd)) && w.bounds.width() > 0 && w.bounds.height() > 0 && onMonitor(w.bounds) {
			problems = append(problems, fmt.Sprintf("window %#x (class %q) of process %d at %+v is on a monitor", w.hwnd, windowClass(w.hwnd), w.pid, w.bounds))
		}
	}
	return problems
}

// smokeWindows answers the visible top-level windows of the smoke's process
// tree, and the window extra when it belongs to the tree, visible or not.
func smokeWindows(extra uintptr) []smokeWindow {
	enumSmoke.Lock()
	defer enumSmoke.Unlock()
	enumSmoke.pids, enumSmoke.extra, enumSmoke.out = smokeProcessTree(), extra, nil
	_ = windows.EnumWindows(enumSmokeCallback, nil)
	return enumSmoke.out
}

// enumSmoke is the one EnumWindows callback's state: a callback is a
// process-lifetime resource (Windows allows a few thousand), so every sample
// shares one.
var enumSmoke struct {
	sync.Mutex
	pids  map[uint32]bool
	extra uintptr
	out   []smokeWindow
}

var enumSmokeCallback = windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(windows.HWND(hwnd), &pid); err != nil || !enumSmoke.pids[pid] {
		return 1
	}
	if hwnd != enumSmoke.extra && !windows.IsWindowVisible(windows.HWND(hwnd)) {
		return 1
	}
	w := smokeWindow{hwnd: hwnd, pid: pid}
	_, _, _ = procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&w.bounds)))
	enumSmoke.out = append(enumSmoke.out, w)
	return 1
})

// windowClass answers a window's class name, which names what opened it.
func windowClass(hwnd uintptr) string {
	var name [256]uint16
	n, err := windows.GetClassName(windows.HWND(hwnd), &name[0], int32(len(name)))
	if err != nil {
		return "?"
	}
	return windows.UTF16ToString(name[:n])
}

// onMonitor answers whether any part of r is on a monitor.
func onMonitor(r rect) bool {
	monitor, _, _ := procMonitorFromRec.Call(uintptr(unsafe.Pointer(&r)), monitorDefaultToNull)
	return monitor != 0
}

// smokeProcessTree answers this process and every process descended from it.
func smokeProcessTree() map[uint32]bool {
	tree := map[uint32]bool{uint32(os.Getpid()): true}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return tree
	}
	defer windows.CloseHandle(snapshot)
	parents := map[uint32]uint32{}
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		parents[entry.ProcessID] = entry.ParentProcessID
	}
	for grew := true; grew; {
		grew = false
		for pid, parent := range parents {
			if !tree[pid] && tree[parent] {
				tree[pid] = true
				grew = true
			}
		}
	}
	return tree
}

// The watcher must find the smoke's windows and recognize a rect on a
// monitor, or a smoke passing under it proves nothing. Checked without
// putting anything on screen: the off-screen smoke window is listed and
// judged off every monitor, and the primary monitor's origin is judged on
// one.
func TestSmokeWindowWatcherSeesTheSmokeWindows(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run the real-window checks")
	}
	ui := startSmokeUIThread(t)
	var found *smokeWindow
	for _, w := range smokeWindows(0) {
		if w.hwnd == ui.hwnd {
			found = &w
		}
	}
	if found == nil {
		t.Fatal("the watcher does not list the smoke window")
	}
	if onMonitor(found.bounds) {
		t.Fatalf("the smoke window at %+v is on a monitor", found.bounds)
	}
	if !onMonitor(rect{Left: 0, Top: 0, Right: 1, Bottom: 1}) {
		t.Fatal("the primary monitor's origin is not judged on a monitor")
	}
	if problems := smokeWindowProblems(); len(problems) != 0 {
		t.Fatalf("problems with only the smoke window up: %v", problems)
	}
}
