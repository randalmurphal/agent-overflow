//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"strconv"
	"unsafe"
)

// heldPopupView is a popup view a WebKit engine holds until the Manager
// adopts or discards it, with the handle of the page that opened it and
// the page number its adoption gives it.
type heldPopupView struct {
	view   unsafe.Pointer
	opener string
	n      uint64
}

// pageHandle is the handle of a WebKit engine's page numbered n. A popup
// is numbered as it is held (holdPopup) and reported and adopted under its
// page's handle, so it has one handle before and after its adoption: a
// popup it opens while held names it as its opener, and the Manager finds
// that opener listed under it. The view keeps n, so a download it starts
// names the popup too.
func pageHandle(n uint64) string {
	return "page-" + strconv.FormatUint(n, 10)
}
