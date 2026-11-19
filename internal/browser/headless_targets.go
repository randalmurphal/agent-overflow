package browser

import (
	"context"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// A headless profile's browser-level target events: popups, crashes and closes.

// subscribe connects the CDP events of the browser connected on browserCtx
// to the seam, and answers the requests the workspace's navigation policy
// paused on it.
//
// A page's handle IS its CDP target id here (there is no second identity to
// re-key onto, unlike the launcher-hosted engine), so the engine's whole
// bookkeeping is which profile owns which target. A target this engine never
// bound is dropped rather than reported under a handle nobody owns.
func (p *headlessProfile) subscribe(browserCtx context.Context) error {
	logf := p.engine.logf
	targets := newHeadlessTargets(p)
	downloads := p.downloadTracker(browserCtx, targets)
	if err := downloads.subscribe(browserCtx, logf); err != nil {
		return err
	}
	session := chromedp.FromContext(browserCtx).Browser
	if err := onBrowserEvent(browserCtx, fetch.RequestPaused, logf, func(event fetch.EventRequestPaused) {
		// A page's main frame id is its target id, so a top-level request
		// names its page, and a popup's is followed by the openers whose
		// authority it has until adoption gives it its own. The Manager
		// resolves a subframe to the page that reported it. A frame no page
		// answers to yet waits for one (awaitRequestOwner).
		frame := string(event.FrameID)
		answerPausedRequest(browserCtx, session, event, func(rawURL string) bool {
			return awaitRequestOwner(browserCtx, p.engine.ownerWait, func() (bool, bool) { return p.allow(targets.owners(frame), rawURL) }, logf)
		})
	}); err != nil {
		return err
	}
	if err := onBrowserEvent(browserCtx, target.TargetCreated, logf, targets.created); err != nil {
		return err
	}
	if err := onBrowserEvent(browserCtx, target.TargetInfoChanged, logf, targets.infoChanged); err != nil {
		return err
	}
	if err := onBrowserEvent(browserCtx, target.TargetCrashed, logf, p.targetCrashed); err != nil {
		return err
	}
	return onBrowserEvent(browserCtx, target.TargetDestroyed, logf, targets.destroyed)
}

// downloadTracker is the tracker of the downloads of the browser connected
// on browserCtx. A popup's main frame id is its target id, which is its
// handle. Chromium sends a popup's creation before the navigation that
// downloads, but the start can be handled first: a start whose frame no
// page owns and that names no popup is offered again until the record
// names the frame's opener or a listed page owns the frame (a popup's
// subframe, once the popup is listed), for up to ownerWait, and refused
// after (awaitOwner). One whose
// popup's destroy the record handled first still names it
// (downloadOwners).
func (p *headlessProfile) downloadTracker(browserCtx context.Context, targets *headlessTargets) *downloadTracker {
	downloads := newDownloadTracker(p.engine.events, refusedDownloadCanceler(p.cancelRefusedDownload, p.engine.logf))
	downloads.popup = func(frame string) downloadStart {
		owners := targets.downloadOwners(frame)
		if len(owners) < 2 {
			return downloadStart{}
		}
		return downloadStart{Popup: frame, Openers: owners[1:], Profile: p.handle}
	}
	downloads.lifetime, downloads.ownerWait = browserCtx, p.engine.ownerWait
	return downloads
}

// targetCrashed reports a page of this profile whose renderer died; the
// Manager closes the target and keeps its tab as a crashed one.
func (p *headlessProfile) targetCrashed(event target.EventTargetCrashed) {
	handle := string(event.TargetID)
	if owner, ok := p.engine.profileForPage(handle); !ok || owner != p {
		return
	}
	// Off the event reader: the Manager closes the page's driver.
	go p.engine.events.PageCrashed(handle, cdpCrashReason(event))
}

// headlessTargets handles the target events of one browser connection, over
// its record of the connection's targets (cdpTargets). A created for a gone
// target neither binds nor reports it: a popup is never reported after it
// is gone. A created that finds held info reports the popup with that newer
// info, or delivers it as an info change for a page the Manager created and
// bound. An info change for a gone target is dropped.
//
// Binding and unbinding happen in the record's parts, under its lock, so a
// popup cannot be bound after the destroyed handler has looked for it.
// PageInfoChanged is delivered outside the lock, numbered under it in the
// order the record handled the changes, so the Manager keeps a newer change
// over a held info delivered after it.
type headlessTargets struct {
	profile *headlessProfile
	*cdpTargets
}

func newHeadlessTargets(p *headlessProfile) *headlessTargets {
	return &headlessTargets{profile: p, cdpTargets: newCDPTargets(nil)}
}

func (h *headlessTargets) created(event target.EventTargetCreated) {
	info := event.TargetInfo
	if info == nil {
		return
	}
	p := h.profile
	handle := string(info.TargetID)
	var deliver func()
	h.cdpTargets.created(info, func(newest *target.Info, held bool, order uint64) {
		// An opener is what makes a target a POPUP: every page the Manager
		// asked for is created without one, and the workers, iframes and
		// service workers Chromium also reports are not pages at all.
		if info.Type != "page" || info.OpenerID == "" {
			// A page the Manager created is bound before its events arrive,
			// so an info change held for it is delivered now.
			if _, bound := p.engine.profileForPage(handle); held && bound {
				deliver = func() { p.engine.events.PageInfoChanged(handle, newest.URL, newest.Title, order) }
			}
			return
		}
		p.engine.bindPage(handle, p)
		// On its own goroutine, as the WebKit engines report popups: the
		// Manager adopts a popup through AttachPage, a CDP round trip that
		// must not hold the target events queued behind it.
		go p.engine.events.PopupOpened(enginePopup{
			Profile: p.handle, Opener: string(info.OpenerID), Handle: handle,
			URL: newest.URL, Title: newest.Title,
		})
	})
	if deliver != nil {
		deliver()
	}
}

func (h *headlessTargets) infoChanged(event target.EventTargetInfoChanged) {
	info := event.TargetInfo
	if info == nil || info.Type != "page" {
		return
	}
	p := h.profile
	handle := string(info.TargetID)
	order, ok := h.changed(info, func() bool {
		if !h.isKnownLocked(info.TargetID) {
			return false
		}
		_, bound := p.engine.profileForPage(handle)
		return bound
	})
	if ok {
		p.engine.events.PageInfoChanged(handle, info.URL, info.Title, order)
	}
}

func (h *headlessTargets) destroyed(event target.EventTargetDestroyed) {
	p := h.profile
	handle := string(event.TargetID)
	var bound bool
	h.cdpTargets.destroyed(event.TargetID, func() {
		// A page the Manager created is bound before its created event may
		// be handled, so the unbind does not depend on the record.
		bound = p.engine.unbindPage(handle)
	})
	if !bound {
		return
	}
	// On its own goroutine for the same reason as the popup above, and as
	// the hosted engine retires a page: PageClosed is the Manager's page
	// teardown, which on the workspace's last page disposes this profile and
	// blocks until Chromium is reaped.
	go p.engine.events.PageClosed(handle)
}
