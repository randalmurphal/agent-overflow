//go:build windows

package webview2host

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	procGetWindow     = user32.NewProc("GetWindow")
	procSetWindowPo   = user32.NewProc("SetWindowPos")
	procGetClientRect = user32.NewProc("GetClientRect")
)

const (
	gwHwndNext = 2
	gwChild    = 5

	hwndTop = 0

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoActivate = 0x0010

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
