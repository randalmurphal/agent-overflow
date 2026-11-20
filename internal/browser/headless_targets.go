package browser

import (
	"context"

	"agent-overflow/internal/browser/cdporder"
	"agent-overflow/internal/browser/ids"

	"github.com/chromedp/cdproto"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// A headless profile's browser-level events: popups, crashes and closes,
// each reported to the Manager as it is handled (engineEvents), and
// downloads.

// subscribe connects the CDP events of the browser connected on browserCtx
// to the seam, and answers the requests the workspace's navigation policy
// paused on it.
//
// A page's handle IS its CDP target id here (there is no second identity to
// re-key onto, unlike the launcher-hosted engine), so the engine's whole
// bookkeeping is which profile owns which target. A target this engine never
// bound is dropped rather than reported under a handle nobody owns.
//
// The target and download events are read from one input in the order
// Chromium sent them (cdporder.WatchBrowser): a target's creation, which
// binds a popup, is handled before its crash or destroy, and a popup's
// creation before a download it starts, so neither is reported before the
// page it names exists.
func (p *headlessProfile) subscribe(browserCtx context.Context, targets *headlessTargets, downloads *downloadTracker) error {
	logf := p.engine.logf
	// The first subscription connects the browser, which the ordered
	// input needs.
	if err := onBrowserEvent(browserCtx, fetch.RequestPaused, logf, func(event fetch.EventRequestPaused) {
		session := chromedp.FromContext(browserCtx).Browser
		// A page's main frame id is its target id, so a top-level request
		// names its page, and a popup's is followed by the openers whose
		// authority it has until adoption gives it its own. The Manager
		// resolves a subframe to the page that reported it. A frame no page
		// answers to yet waits for one (awaitRequestOwner).
		frame := frameID(event.FrameID)
		answerPausedRequest(browserCtx, session, event, func(rawURL string) bool {
			return awaitRequestOwner(browserCtx, p.engine.ownerWait, func() (bool, bool) { return p.allow(targets.owners(frame), rawURL) }, logf)
		})
	}); err != nil {
		return err
	}
	return cdporder.WatchBrowser(browserCtx, browserMethods, func(m cdporder.Message) {
		p.browserEvent(m, targets, downloads)
	})
}

// browserMethods are what a headless browser's ordered input carries
// (browserEvent).
var browserMethods = []string{
	string(cdproto.EventTargetTargetCreated), string(cdproto.EventTargetTargetInfoChanged),
	string(cdproto.EventTargetTargetCrashed), string(cdproto.EventTargetTargetDestroyed),
	string(cdproto.EventBrowserDownloadWillBegin), string(cdproto.EventBrowserDownloadProgress),
}

// browserEvent hands one message of the browser's ordered input to its
// handler, on the input's goroutine. One that does not decode is logged.
func (p *headlessProfile) browserEvent(m cdporder.Message, targets *headlessTargets, downloads *downloadTracker) {
	decode := func(out any) bool {
		if err := m.Decode(out); err != nil {
			p.engine.logf("browser: profile %s: %s: %v", p.handle, m.Method, err)
			return false
		}
		return true
	}
	switch cdproto.MethodType(m.Method) {
	case cdproto.EventTargetTargetCreated:
		var event target.EventTargetCreated
		if decode(&event) {
			targets.created(event)
		}
	case cdproto.EventTargetTargetInfoChanged:
		var event target.EventTargetInfoChanged
		if decode(&event) {
			targets.infoChanged(event)
		}
	case cdproto.EventTargetTargetCrashed:
		var event target.EventTargetCrashed
		if decode(&event) {
			p.targetCrashed(event)
		}
	case cdproto.EventTargetTargetDestroyed:
		var event target.EventTargetDestroyed
		if decode(&event) {
			targets.destroyed(event)
		}
	case cdproto.EventBrowserDownloadWillBegin:
		var event cdpbrowser.EventDownloadWillBegin
		if decode(&event) {
			downloads.willBegin(event)
		}
	case cdproto.EventBrowserDownloadProgress:
		var event cdpbrowser.EventDownloadProgress
		if decode(&event) {
			downloads.progress(event)
		}
	}
}

// downloadTracker is the tracker of the downloads of the browser connected
// on browserCtx. A popup's main frame id is its target id, which is its
// handle, and its creation is handled before a download it starts. A start
// whose frame no page owns and that names no popup (a popup's subframe
// before the popup is listed) is offered again until a listed page owns
// the frame, for up to ownerWait, and refused after (awaitOwner). One
// whose popup's destroy was handled first still names it (downloadOwners).
// Each download's files in the profile's download directory stay claimed
// until its writer stops (downloadTracker).
func (p *headlessProfile) downloadTracker(browserCtx context.Context, targets *headlessTargets) *downloadTracker {
	downloads := newDownloadTracker(p.engine.events, refusedDownloadCanceler(p.CancelDownload, p.engine.logf), p.removeDownloadFile)
	downloads.popup = func(frame frameID) downloadStart {
		owners := targets.downloadOwners(frame)
		if len(owners) < 2 {
			return downloadStart{}
		}
		return downloadStart{Popup: owners[0], Openers: owners[1:], Profile: p.handle}
	}
	downloads.lifetime, downloads.ownerWait = browserCtx, p.engine.ownerWait
	return downloads
}

// targetCrashed reports a page of this profile whose renderer died; the
// Manager closes the target and keeps its tab as a crashed one.
func (p *headlessProfile) targetCrashed(event target.EventTargetCrashed) {
	handle := ids.PageHandle(event.TargetID)
	if owner, ok := p.engine.profileForPage(handle); !ok || owner != p {
		return
	}
	p.engine.events.PageCrashed(handle, cdpCrashReason(event))
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
	handle := ids.PageHandle(info.TargetID)
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
		// Bound and reported in the record's hold, so the destroyed
		// handler, which unbinds in it too, reports the popup's close
		// after its report. The Manager adopts it on a goroutine of its
		// own (engineEvents).
		p.engine.bindPage(handle, p)
		p.engine.events.PopupOpened(enginePopup{
			Profile: p.handle, Opener: ids.PageHandle(info.OpenerID), Handle: handle,
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
	handle := ids.PageHandle(info.TargetID)
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
	handle := ids.PageHandle(event.TargetID)
	var bound bool
	h.cdpTargets.destroyed(event.TargetID, func() {
		// A page the Manager created is bound before its created event may
		// be handled, so the unbind does not depend on the record.
		bound = p.engine.unbindPage(handle)
	})
	if !bound {
		return
	}
	p.engine.events.PageClosed(handle)
}
