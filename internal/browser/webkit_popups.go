//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"strconv"
	"sync"
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

// pageEnd is what ended a page's view: the reason its web process ended,
// and whether it closed itself (window.close()).
type pageEnd struct {
	crashed string
	closed  bool
}

func (e pageEnd) merge(other pageEnd) pageEnd {
	if e.crashed == "" {
		e.crashed = other.crashed
	}
	e.closed = e.closed || other.closed
	return e
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

// pageEnds is a WebKit page's crash and close. The Manager acts on those
// only for a listed page, so an adopted popup's are held from its attach
// until its listing (Adopted) and delivered then, the crash first. A close
// also reaches the Manager at once, as the hosted engine's does, so what
// the popup downloaded is listed while its opener still is. Every other
// page's go straight to the Manager.
type pageEnds struct {
	mu sync.Mutex
	// popup is set before the view answers to the page, for a popup.
	popup, listed bool
	held          pageEnd
}

// report hands end to the Manager, or holds it for the listing. It runs
// on the UI thread, so the Manager hears of it on a goroutine of its own:
// it closes the view through the UI thread.
func (p *pageEnds) report(end pageEnd, events engineEvents, handle string) {
	p.mu.Lock()
	hold := p.popup && !p.listed
	if hold {
		p.held = p.held.merge(end)
	}
	p.mu.Unlock()
	switch {
	case hold && end.closed:
		go events.PageClosed(handle)
	case hold:
	case end.crashed != "":
		go events.PageCrashed(handle, end.crashed)
	case end.closed:
		go events.PageClosed(handle)
	}
}

// adopted delivers what ended the popup before its listing, once the
// Manager lists it.
func (p *pageEnds) adopted(events engineEvents, handle string) {
	p.mu.Lock()
	p.listed = true
	held := p.held
	p.held = pageEnd{}
	p.mu.Unlock()
	if held.crashed != "" {
		events.PageCrashed(handle, held.crashed)
	}
	if held.closed {
		events.PageClosed(handle)
	}
}

// hold keeps end, which reached the engine before the page's attach, for
// the listing.
func (p *pageEnds) hold(end pageEnd) {
	p.mu.Lock()
	p.held = p.held.merge(end)
	p.mu.Unlock()
}
