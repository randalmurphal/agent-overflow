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

// heldPopupEnds is a WebKit engine's record of what ended a popup's view
// before the view answers to a page: from the popup's creation until its
// adoption names the view's page on the UI thread, a crash or close reaches
// the engine by the popup's number and is kept here by its handle, for the
// popup held (holdPopup) or being attached. Its page takes it at its
// attach (take). Guarded by the engine's lock.
type heldPopupEnds struct {
	ends      map[string]pageEnd
	attaching map[string]struct{}
}

// recordLocked keeps end for the popup handle if it is held or being
// attached, and reports whether it did. held says the engine holds it.
func (h *heldPopupEnds) recordLocked(handle string, held bool, end pageEnd) bool {
	if _, attaching := h.attaching[handle]; !held && !attaching {
		return false
	}
	if h.ends == nil {
		h.ends = make(map[string]pageEnd)
	}
	h.ends[handle] = h.ends[handle].merge(end)
	return true
}

// attachLocked marks handle as being attached, taken from the held popups.
func (h *heldPopupEnds) attachLocked(handle string) {
	if h.attaching == nil {
		h.attaching = make(map[string]struct{})
	}
	h.attaching[handle] = struct{}{}
}

// takeLocked ends handle's attach and answers what ended it meanwhile.
func (h *heldPopupEnds) takeLocked(handle string) pageEnd {
	end := h.ends[handle]
	delete(h.ends, handle)
	delete(h.attaching, handle)
	return end
}

// forgetLocked drops what is kept for a popup discarded.
func (h *heldPopupEnds) forgetLocked(handle string) {
	delete(h.ends, handle)
	delete(h.attaching, handle)
}
