//go:build windows

package webview2host

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32             = windows.NewLazySystemDLL("user32.dll")
	procGetWindow      = user32.NewProc("GetWindow")
	procSetWindowPo    = user32.NewProc("SetWindowPos")
	procGetClientRect  = user32.NewProc("GetClientRect")
	procRegisterClass  = user32.NewProc("RegisterClassExW")
	procCreateWindowEx = user32.NewProc("CreateWindowExW")
	procDestroyWindow  = user32.NewProc("DestroyWindow")
	procShowWindow     = user32.NewProc("ShowWindow")
	procDefWindowProc  = user32.NewProc("DefWindowProcW")
)

const (
	gwHwndNext = 2
	gwChild    = 5

	hwndTop = 0

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010

	wsChild        = 0x40000000
	wsClipSiblings = 0x04000000
	wsClipChildren = 0x02000000

	swHide   = 0
	swShowNA = 8

	errorClassAlreadyExists = 1410

	// maxHostChildren bounds the child walk. The launcher window holds
	// the SPA controller plus one child per browser tab, so this is two
	// orders of magnitude of headroom; it exists so a corrupt sibling
	// chain cannot spin the walk forever.
	maxHostChildren = 256
)

// clientSize answers hwnd's client area in physical pixels, or ok=false
// for a window the call cannot read (destroyed, zero).
func clientSize(hwnd uintptr) (width, height int32, ok bool) {
	if hwnd == 0 {
		return 0, 0, false
	}
	var r rect
	ret, _, _ := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if ret == 0 {
		return 0, 0, false
	}
	return r.Right - r.Left, r.Bottom - r.Top, true
}

// childWindows walks hwnd's direct children in top-to-bottom z-order.
func childWindows(hwnd uintptr) []uintptr {
	var out []uintptr
	child, _, _ := procGetWindow.Call(hwnd, gwChild)
	for child != 0 && len(out) < maxHostChildren {
		out = append(out, child)
		child, _, _ = procGetWindow.Call(child, gwHwndNext)
	}
	return out
}

// newChildWindow returns the one child of hwnd present in after but not
// before, or 0 when the diff is not exactly one window.
//
// This is how the controller's child HWND is found: WebView2 does not
// expose it, and the host needs it for the z-order fix below.
func newChildWindow(before []uintptr, after []uintptr) uintptr {
	seen := make(map[uintptr]struct{}, len(before))
	for _, h := range before {
		seen[h] = struct{}{}
	}
	var found uintptr
	for _, h := range after {
		if _, existed := seen[h]; existed {
			continue
		}
		if found != 0 {
			// Two new children means something other than this
			// controller also created a window; picking either would be
			// a guess, and raising the wrong one would hide the pane.
			return 0
		}
		found = h
	}
	return found
}

// raiseChild puts a WebView2 controller's child window at the top of the
// host's child z-order.
//
// Load-bearing, and invisible when missing: WebView2 inserts every new
// controller window at the BOTTOM of the host's child z-order, so the
// SPA's controller (created first, by Wails) paints over every pane
// controller. The spike measured a fully visible pane as byte-identical
// to no pane at all until this call ran. One call per controller, after
// creation.
func raiseChild(child uintptr) {
	if child == 0 {
		return
	}
	_, _, _ = procSetWindowPo.Call(child, hwndTop, 0, 0, 0, 0, swpNoSize|swpNoMove|swpNoActivate)
}

// ---------------------------------------------------------------------
// The clip container
// ---------------------------------------------------------------------

// clipClassName is the window class of the per-page clip container. It is
// the only window this process registers a class for, so the name only has
// to be unique within the process's module.
const clipClassName = "AgentOverflowBrowserPaneClip"

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

var (
	clipClassOnce sync.Once
	clipClassErr  error
	clipModule    windows.Handle
	// clipWndProc is registered once and lives for the process. The
	// container has no behaviour of its own: it exists to be a clipping
	// rectangle, so every message goes straight to DefWindowProc.
	clipWndProc = windows.NewCallback(func(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
		ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wparam, lparam)
		return ret
	})
)

// registerClipClass registers the container's window class once per
// process. Runs on the UI thread, like every other window call here.
func registerClipClass() error {
	clipClassOnce.Do(func() {
		if err := windows.GetModuleHandleEx(0, nil, &clipModule); err != nil {
			clipClassErr = fmt.Errorf("module handle: %w", err)
			return
		}
		name, err := windows.UTF16PtrFromString(clipClassName)
		if err != nil {
			clipClassErr = err
			return
		}
		class := wndClassExW{
			cbSize:      uint32(unsafe.Sizeof(wndClassExW{})),
			lpfnWndProc: clipWndProc,
			hInstance:   clipModule,
			// A NULL background brush is load-bearing: DefWindowProc then
			// erases nothing, so the container never paints a frame of its
			// own colour underneath the page it is clipping. With a brush
			// here every move would flash that colour through the pane.
			hbrBackground: 0,
			lpszClassName: name,
		}
		atom, _, callErr := procRegisterClass.Call(uintptr(unsafe.Pointer(&class)))
		if atom == 0 && !isClassAlreadyExists(callErr) {
			clipClassErr = fmt.Errorf("register %s window class: %w", clipClassName, callErr)
		}
	})
	return clipClassErr
}

// isClassAlreadyExists tolerates a re-registration. The class is
// process-wide and registered under a sync.Once, so this only fires if
// something else in the process already claimed the name — in which case
// the existing class is equivalent and CreateWindowEx works either way.
func isClassAlreadyExists(err error) bool {
	return errors.Is(err, windows.Errno(errorClassAlreadyExists))
}

// createClipContainer creates one page's clip container as a hidden child
// of parent.
//
// WS_CLIPCHILDREN keeps the container from painting over the controller
// window inside it; WS_CLIPSIBLINGS keeps it from painting over the SPA's
// own WebView2, which is a sibling. It starts hidden because pages start
// hidden — a visible empty container would eat mouse input over its whole
// rectangle while showing nothing.
func createClipContainer(parent uintptr) (uintptr, error) {
	if parent == 0 {
		return 0, fmt.Errorf("clip container needs a parent window")
	}
	if err := registerClipClass(); err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(clipClassName)
	if err != nil {
		return 0, err
	}
	hwnd, _, callErr := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(name)),
		0,
		wsChild|wsClipChildren|wsClipSiblings,
		0, 0, 0, 0,
		parent,
		0,
		uintptr(clipModule),
		0,
	)
	if hwnd == 0 {
		return 0, fmt.Errorf("create clip container: %w", callErr)
	}
	return hwnd, nil
}

// moveWindow positions a window in its parent's client coordinates
// without touching z-order or activation.
func moveWindow(hwnd uintptr, r rect) {
	if hwnd == 0 {
		return
	}
	_, _, _ = procSetWindowPo.Call(
		hwnd, 0,
		uintptr(int32(r.Left)), uintptr(int32(r.Top)),
		uintptr(int32(r.width())), uintptr(int32(r.height())),
		swpNoZOrder|swpNoActivate,
	)
}

// showWindow shows or hides a window without activating it.
func showWindow(hwnd uintptr, visible bool) {
	if hwnd == 0 {
		return
	}
	cmd := uintptr(swHide)
	if visible {
		cmd = swShowNA
	}
	_, _, _ = procShowWindow.Call(hwnd, cmd)
}

// destroyWindow tears a container down. A leaked container is invisible
// and eats every click over its rectangle, so this runs on every close
// path.
func destroyWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	_, _, _ = procDestroyWindow.Call(hwnd)
}
