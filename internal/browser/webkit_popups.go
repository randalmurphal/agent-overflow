//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"strconv"
	"unsafe"
)

// heldPopupView is a popup view a WebKit engine holds until the Manager
// adopts or discards it, with the handle of the page that opened it.
type heldPopupView struct {
	view   unsafe.Pointer
	opener string
}

// popupHandle is the handle a WebKit engine reports its n-th popup under,
// which the Manager adopts it under. The view keeps n, so a download it
// starts names the popup before and after adoption.
func popupHandle(n uint64) string {
	return "popup-" + strconv.FormatUint(n, 10)
}
