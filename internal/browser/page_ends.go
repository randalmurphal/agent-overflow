package browser

import "sync"

// pageEnd is what ended a page's view: the reason its process ended, and
// whether it closed (window.close(), or its target went).
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

// pageEnds is a page's crash and close, as the WebKit engines and headless
// Chromium report them. The Manager acts on those only for a listed page,
// so an adopted popup's are held from its creation until its listing
// (adoptedPage) and delivered then: its close if it closed, its crash
// otherwise. A crash delivered first would keep the tab as a crashed one
// (crash.go), which the close, looking only among live pages, could not
// remove. A close also reaches
// the Manager at once, as the hosted engine's does, so what the popup
// downloaded is listed while its opener still is. Every other page's go
// straight to the Manager.
type pageEnds struct {
	mu sync.Mutex
	// popup is set before the page answers to its handle, for a popup.
	popup, listed bool
	held          pageEnd
}

// keep holds end for the listing if the page is a popup not listed yet,
// and answers whether it did.
func (p *pageEnds) keep(end pageEnd) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.popup || p.listed {
		return false
	}
	p.held = p.held.merge(end)
	return true
}

// report hands end to the Manager, or holds it for the listing. It may run
// on a UI thread or an event reader, so the Manager hears of it on a
// goroutine of its own: it closes the page through them.
func (p *pageEnds) report(end pageEnd, events engineEvents, handle string) {
	hold := p.keep(end)
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
	switch {
	case held.closed:
		events.PageClosed(handle)
	case held.crashed != "":
		events.PageCrashed(handle, held.crashed)
	}
}

// hold keeps end, which reached the engine before the page's attach, for
// the listing.
func (p *pageEnds) hold(end pageEnd) {
	p.mu.Lock()
	p.held = p.held.merge(end)
	p.mu.Unlock()
}
