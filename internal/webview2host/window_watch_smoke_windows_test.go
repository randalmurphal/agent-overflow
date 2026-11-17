//go:build windows

package webview2host

import (
	"errors"
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
	procIsWindow           = user32.NewProc("IsWindow")
	procGetClassName       = user32.NewProc("GetClassNameW")
	procGetWindowRect      = user32.NewProc("GetWindowRect")
)

// gwOwner is GetWindow's GW_OWNER.
const gwOwner = 4

// smokeWindowSeen is a visible top-level window the watch saw, from when
// it was first seen until it was last seen. A window gone before its class
// and rect were read has neither.
type smokeWindowSeen struct {
	hwnd        uintptr
	class       string
	owner       uintptr
	rect        rect
	shown, gone time.Time
}

func (w smokeWindowSeen) String() string {
	shown := "still shown"
	if !w.gone.IsZero() {
		shown = fmt.Sprintf("shown %v", w.gone.Sub(w.shown).Round(time.Millisecond))
	}
	return fmt.Sprintf("%#x %q owned by %#x, %dx%d at (%d,%d), %s", w.hwnd, w.class, w.owner, w.rect.width(), w.rect.height(), w.rect.Left, w.rect.Top, shown)
}

// webView2Window reports whether w is a window WebView2 draws over the
// smoke's window own: Chromium's widget class, owned by own. Measured on
// runtime 157: the download dialog it opens over a page is one.
func (w smokeWindowSeen) webView2Window(own uintptr) bool {
	return w.class == "Chrome_WidgetWin_1" && w.owner == own
}

// statusBubble reports whether w is WebView2's link status bubble, the
// strip it shows over a page's bottom corner while a link is hovered: a
// window of its over own, at most 40 pixels high.
func (w smokeWindowSeen) statusBubble(own uintptr) bool {
	return w.webView2Window(own) && w.rect.height() <= 40
}

// smokeWindowWatch polls the desktop's visible top-level windows every
// millisecond and keeps every one that appeared after its baseline, which
// must include the smoke's own window.
type smokeWindowWatch struct {
	own     uintptr
	mu      sync.Mutex
	current map[uintptr]*smokeWindowSeen
	seen    []*smokeWindowSeen
	err     error
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

// watchSmokeWindows starts the watch once a baseline enumeration that sees
// the smoke's window own has succeeded, and fails the test otherwise.
func watchSmokeWindows(t *testing.T, own uintptr) *smokeWindowWatch {
	t.Helper()
	w := &smokeWindowWatch{own: own, current: map[uintptr]*smokeWindowSeen{}}
	stop := make(chan struct{})
	done := make(chan struct{})
	baseline := make(chan error, 1)
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		desktop, _, callErr := procGetThreadDesktop.Call(uintptr(windows.GetCurrentThreadId()))
		if desktop == 0 {
			baseline <- fmt.Errorf("GetThreadDesktop: %w", callErr)
			return
		}
		found, err := enumSmokeWindows(desktop)
		if err == nil && !containsWindow(found, own) {
			err = fmt.Errorf("the baseline of %d windows does not include the smoke's window %#x", len(found), own)
		}
		if err != nil {
			baseline <- err
			return
		}
		for _, hwnd := range found {
			w.current[hwnd] = &smokeWindowSeen{hwnd: hwnd}
		}
		baseline <- nil
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if err := w.poll(desktop); err != nil {
				w.mu.Lock()
				w.err = errors.Join(w.err, err)
				w.mu.Unlock()
				return
			}
		}
	}()
	if err := <-baseline; err != nil {
		<-done
		t.Fatalf("the window watch has no baseline: %v", err)
	}
	t.Cleanup(func() { close(stop); <-done })
	return w
}

func containsWindow(found []uintptr, hwnd uintptr) bool {
	for _, f := range found {
		if f == hwnd {
			return true
		}
	}
	return false
}

// enumSmokeWindows answers the desktop's visible top-level windows.
func enumSmokeWindows(desktop uintptr) ([]uintptr, error) {
	smokeEnumFound = smokeEnumFound[:0]
	if ok, _, err := procEnumDesktopWindows.Call(desktop, smokeEnum, 0); ok == 0 {
		return nil, fmt.Errorf("EnumDesktopWindows: %w", err)
	}
	return append([]uintptr(nil), smokeEnumFound...), nil
}

// poll takes one enumeration: a window not seen before is read, and one no
// longer there is marked gone. A window that cannot be read while it still
// exists is the watch's error.
func (w *smokeWindowWatch) poll(desktop uintptr) error {
	found, err := enumSmokeWindows(desktop)
	if err != nil {
		return err
	}
	now := time.Now()
	present := map[uintptr]bool{}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, hwnd := range found {
		present[hwnd] = true
		if _, ok := w.current[hwnd]; ok {
			continue
		}
		seen, err := readSmokeWindow(hwnd)
		if err != nil {
			return err
		}
		seen.shown = now
		w.current[hwnd] = seen
		w.seen = append(w.seen, seen)
	}
	for hwnd, seen := range w.current {
		if !present[hwnd] {
			seen.gone = now
			delete(w.current, hwnd)
		}
	}
	return nil
}

// readSmokeWindow reads a window's class, owner and rect. One destroyed
// meanwhile is answered with neither class nor rect, still a window seen.
func readSmokeWindow(hwnd uintptr) (*smokeWindowSeen, error) {
	seen := &smokeWindowSeen{hwnd: hwnd}
	var class [256]uint16
	n, _, classErr := procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
	ok, _, rectErr := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&seen.rect)))
	seen.owner, _, _ = procGetWindow.Call(hwnd, gwOwner)
	if n != 0 && ok != 0 {
		seen.class = windows.UTF16ToString(class[:n])
		return seen, nil
	}
	if alive, _, _ := procIsWindow.Call(hwnd); alive != 0 {
		return nil, fmt.Errorf("read window %#x: class: %v, rect: %v", hwnd, classErr, rectErr)
	}
	seen.rect = rect{}
	return seen, nil
}

// take answers the windows seen since the last call, or the watch's error.
func (w *smokeWindowWatch) take() ([]smokeWindowSeen, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	out := make([]smokeWindowSeen, 0, len(w.seen))
	for _, seen := range w.seen {
		out = append(out, *seen)
	}
	w.seen = nil
	return out, nil
}

// A window is allowed by its class and owner, never by its size alone.
func TestSmokeWindowsAreAllowedByClassAndOwner(t *testing.T) {
	const own = 0x10
	bubble := smokeWindowSeen{class: "Chrome_WidgetWin_1", owner: own, rect: rect{Right: 300, Bottom: 20}}
	if !bubble.statusBubble(own) {
		t.Error("WebView2's status bubble over the smoke's window was not allowed")
	}
	for _, w := range []smokeWindowSeen{
		{class: "Chrome_WidgetWin_1", owner: 0x20, rect: bubble.rect},
		{class: "Chrome_WidgetWin_1", rect: bubble.rect},
		{class: "#32770", owner: own, rect: bubble.rect},
		{owner: own, rect: bubble.rect},
	} {
		if w.statusBubble(own) || w.webView2Window(own) {
			t.Errorf("%s was allowed", w)
		}
	}
	dialog := smokeWindowSeen{class: "Chrome_WidgetWin_1", owner: own, rect: rect{Right: 388, Bottom: 186}}
	if dialog.statusBubble(own) || !dialog.webView2Window(own) {
		t.Error("a dialog-sized window of WebView2's was taken for the status bubble, or not for WebView2's")
	}
}
