//go:build windows

package webview2host

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A smoke's watch of the visible top-level windows on its desktop, which
// sees a window of WebView2's own as it appears and goes.

var (
	procEnumDesktopWindows = user32.NewProc("EnumDesktopWindows")
	procIsWindowVisible    = user32.NewProc("IsWindowVisible")
	procGetClassName       = user32.NewProc("GetClassNameW")
	procGetWindowRect      = user32.NewProc("GetWindowRect")
)

// smokeWindowSeen is a visible top-level window the watch saw, from when
// it was first seen until it was last seen.
type smokeWindowSeen struct {
	hwnd        uintptr
	class       string
	rect        rect
	shown, gone time.Time
}

func (w smokeWindowSeen) String() string {
	shown := "still shown"
	if !w.gone.IsZero() {
		shown = fmt.Sprintf("shown %v", w.gone.Sub(w.shown).Round(time.Millisecond))
	}
	return fmt.Sprintf("%#x %s %dx%d at (%d,%d), %s", w.hwnd, w.class, w.rect.width(), w.rect.height(), w.rect.Left, w.rect.Top, shown)
}

// statusBubble reports whether w is WebView2's link status bubble, a strip
// it shows over a page's bottom corner while a link is hovered.
func (w smokeWindowSeen) statusBubble() bool {
	return w.rect.height() <= 40
}

// smokeWindowWatch polls the desktop's visible top-level windows every
// millisecond and keeps every one but the smoke's own.
type smokeWindowWatch struct {
	mu      sync.Mutex
	current map[uintptr]*smokeWindowSeen
	seen    []*smokeWindowSeen
}

// smokeEnumWindows is the one enumeration callback, since a process makes
// only a bounded number; the watch's goroutine alone calls it, through
// smokeEnumFound.
var (
	smokeEnumFound []uintptr
	smokeEnum      = windows.NewCallback(func(hwnd, _ uintptr) uintptr {
		if visible, _, _ := procIsWindowVisible.Call(hwnd); visible != 0 {
			smokeEnumFound = append(smokeEnumFound, hwnd)
		}
		return 1
	})
)

func watchSmokeWindows(t *testing.T, own uintptr) *smokeWindowWatch {
	w := &smokeWindowWatch{current: map[uintptr]*smokeWindowSeen{}}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		desktop, _, _ := procGetThreadDesktop.Call(uintptr(windows.GetCurrentThreadId()))
		for {
			select {
			case <-stop:
				return
			default:
			}
			smokeEnumFound = smokeEnumFound[:0]
			_, _, _ = procEnumDesktopWindows.Call(desktop, smokeEnum, 0)
			now := time.Now()
			present := map[uintptr]bool{}
			w.mu.Lock()
			for _, hwnd := range smokeEnumFound {
				if hwnd == own {
					continue
				}
				present[hwnd] = true
				if _, ok := w.current[hwnd]; ok {
					continue
				}
				var class [256]uint16
				_, _, _ = procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
				seen := &smokeWindowSeen{hwnd: hwnd, class: windows.UTF16ToString(class[:]), shown: now}
				_, _, _ = procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&seen.rect)))
				w.current[hwnd] = seen
				w.seen = append(w.seen, seen)
			}
			for hwnd, seen := range w.current {
				if !present[hwnd] {
					seen.gone = now
					delete(w.current, hwnd)
				}
			}
			w.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
	return w
}

// take answers the windows seen since the last call.
func (w *smokeWindowWatch) take() []smokeWindowSeen {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]smokeWindowSeen, 0, len(w.seen))
	for _, seen := range w.seen {
		out = append(out, *seen)
	}
	w.seen = nil
	return out
}
