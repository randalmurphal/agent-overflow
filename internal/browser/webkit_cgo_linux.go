//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

/*
#cgo pkg-config: gtk4 webkitgtk-6.0

#include <stdlib.h>
#include "webkitglue_linux.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	"agent-overflow/internal/browser/destination"
	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/uicall"
	"agent-overflow/internal/keybindings"
)

// The only cgo in the WebKitGTK engine. Everything above it — the engine, the
// profile, the page driver — is ordinary Go against the small Go-typed surface
// declared here, so exactly one file has to be read with C in mind.
//
// Two rules this file exists to enforce:
//
//  1. EVERY GTK or WebKit call happens on the GTK main thread, through
//     `uiThread`: Run for an operation, Release for giving up what is held.
//     Wails owns that thread; calling into GTK from a Go goroutine is
//     undefined behaviour, not a race that shows up under load.
//  2. No Go pointer is ever handed to C. Callbacks address a page or a profile
//     by a uint64 id and this file resolves it, which is also what makes a
//     callback arriving after teardown a lookup miss rather than a crash.

// gtkOnMainThread answers whether a closure posted to uiThread would run
// inline right now: true only on the GTK thread, where Wails' dispatch
// short-circuits.
func gtkOnMainThread() bool { return C.ao_wk_on_main_thread() == 1 }

var errGTKUnavailable = fmt.Errorf("browser: the desktop window is not accepting browser work")

// errProfileClosedBeforeView is a new page's answer when its profile was
// disposed before its view was built (webkitNewView).
var errProfileClosedBeforeView = errors.New("browser: workspace profile closed before the page's view was built")

// webkitView is a live page's view as the page's operations reach it. The
// page's lifetime ends (webkitPage.discard) before Close queues the view's
// release, and the GTK thread runs dispatches in order, so a dispatch that
// still finds the lifetime live there runs before the release.
type webkitView struct {
	ptr  unsafe.Pointer
	life context.Context
}

// do runs fn with the view on the GTK thread, unless the page closed first
// or the thread did not take it in time, after which fn never runs.
func (v webkitView) do(fn func(view unsafe.Pointer)) error {
	closed := false
	if uiThread.Run(func() {
		if v.life.Err() != nil {
			closed = true
			return
		}
		fn(v.ptr)
	}) == uicall.Withdrawn {
		return errGTKUnavailable
	}
	if closed {
		return errViewClosed
	}
	return nil
}

// ---------------------------------------------------------------------------
// Registries the C callbacks resolve against
// ---------------------------------------------------------------------------

var (
	webkitPageSeq     atomic.Uint64
	webkitProfileSeq  atomic.Uint64
	webkitCallSeq     atomic.Uint64
	webkitPageByID    sync.Map // uint64 -> *webkitPage
	webkitProfileByID sync.Map // uint64 -> *webkitProfile
	webkitCallByID    sync.Map // uint64 -> chan webkitCallResult
)

// webkitCallResult is one completed asynchronous WebKit call. Only one of the
// three shapes is populated, decided by which call was issued.
type webkitCallResult struct {
	json   string
	err    string
	pixels []byte
	width  int
	height int
	// found and matches are a find's answer: found 1 with the match count,
	// 0 with none, -1 when the view has no find controller.
	found   int
	matches uint32
}

func webkitRegisterCall() (uint64, chan webkitCallResult) {
	id := webkitCallSeq.Add(1)
	ch := make(chan webkitCallResult, 1)
	webkitCallByID.Store(id, ch)
	return id, ch
}

func webkitCompleteCall(id uint64, result webkitCallResult) {
	value, ok := webkitCallByID.LoadAndDelete(id)
	if !ok {
		return
	}
	// Buffered by one and delivered once: a caller that already gave up on its
	// deadline leaves nothing blocked here.
	value.(chan webkitCallResult) <- result
}

// webkitTakeString copies a GLib-allocated string and releases it. Every
// char* the C half hands over is owned by the Go half from that point on.
func webkitTakeString(text *C.char) string {
	if text == nil {
		return ""
	}
	value := C.GoString(text)
	C.ao_wk_free(unsafe.Pointer(text))
	return value
}

//export aoWebKitEvalDone
func aoWebKitEvalDone(callID C.uint64_t, jsonText *C.char, errText *C.char) {
	webkitCompleteCall(uint64(callID), webkitCallResult{
		json: webkitTakeString(jsonText), err: webkitTakeString(errText),
	})
}

//export aoWebKitFindDone
func aoWebKitFindDone(callID C.uint64_t, found C.int, matchCount C.uint) {
	webkitCompleteCall(uint64(callID), webkitCallResult{found: int(found), matches: uint32(matchCount)})
}

//export aoWebKitSnapshotDone
func aoWebKitSnapshotDone(callID C.uint64_t, pixels unsafe.Pointer, width, height, stride C.int, errText *C.char) {
	result := webkitCallResult{width: int(width), height: int(height), err: webkitTakeString(errText)}
	if pixels != nil {
		result.pixels = C.GoBytes(pixels, C.int(int(stride)*int(height)))
		C.ao_wk_free(pixels)
	}
	webkitCompleteCall(uint64(callID), result)
}

//export aoWebKitAllow
func aoWebKitAllow(pageID C.uint64_t, decision unsafe.Pointer, uri *C.char) {
	target := webkitTakeString(uri)
	page := uint64(pageID)
	// Answered OFF the GTK thread: navigation authority is the Manager's, and
	// asking it takes Manager locks — blocking the GTK thread on a Go lock is
	// how the whole window freezes behind one browser operation. The delegate
	// deferred the decision with a reference held for exactly this.
	go func() {
		allow := C.int(0)
		if webkitNavigationAllowed(page, target) {
			allow = 1
		}
		// The decision's reference is given up with its answer. A loop that
		// is gone takes the page with it, so an unanswered decision dies
		// with the process rather than blocking anything.
		releasePolicyAnswer.release(uiThread, func() { C.ao_wk_policy_finish(decision, allow) })
	}()
}

// webkitNavigationAllowed answers one navigation in a view with the policy
// of the page the glue names for it (ao_view_policy_page_id): the view's own
// page, or for a popup the Manager has not adopted yet, which loads from
// creation, its opener's. The opener's workspace authority is the one the
// adopted popup gets, so the file policy is the same before and after
// adoption, whatever cookie jar the pages are in. A page that is gone, or a
// view with none, refuses.
func webkitNavigationAllowed(pageID uint64, target string) bool {
	page := webkitLookupPage(pageID)
	return page != nil && (page.hooks.Allow == nil || page.hooks.Allow(target))
}

//export aoWebKitConsole
func aoWebKitConsole(pageID C.uint64_t, payload *C.char) {
	text := webkitTakeString(payload)
	if page := webkitLookupPage(uint64(pageID)); page != nil {
		page.consoleMessage(text)
	}
}

//export aoWebKitPageInfo
func aoWebKitPageInfo(pageID C.uint64_t, uri *C.char, title *C.char) {
	location, name := webkitTakeString(uri), webkitTakeString(title)
	page := webkitLookupPage(uint64(pageID))
	if page == nil {
		return
	}
	page.noteLoad()
	if page.hooks.Info == nil {
		return
	}
	// Numbered here, on the GTK thread, in the order the view changed; the
	// Manager takes its own locks, so the report leaves the thread.
	order := page.infoOrder.Add(1)
	go page.hooks.Info(location, name, order)
}

//export aoWebKitPageLoading
func aoWebKitPageLoading(pageID C.uint64_t, loading C.int) {
	page := webkitLookupPage(uint64(pageID))
	if page == nil || page.hooks.Loading == nil {
		return
	}
	// Numbered here, on the GTK thread, in the order the view changed; the
	// Manager takes its own locks, so the report leaves the thread.
	order := page.loadOrder.Add(1)
	go page.hooks.Loading(loading != 0, order)
}

//export aoWebKitDocumentCommitted
func aoWebKitDocumentCommitted(pageID C.uint64_t) {
	page := webkitLookupPage(uint64(pageID))
	if page == nil || page.hooks.DocumentCommitted == nil {
		return
	}
	// The commit's order is stamped here, on the GTK thread, before any later
	// page operation can be dispatched; finish only submits the find's end,
	// which the Manager applies off this thread.
	if finish := page.hooks.DocumentCommitted(); finish != nil {
		finish()
	}
}

// aoWebKitKeyChord runs ON the GTK thread, inside the key event's delivery,
// and answers synchronously: it is a set lookup in the Manager and nothing
// that waits. The routing that follows a claim, or an Escape's
// announcement, leaves the thread on a goroutine.
//
//export aoWebKitKeyChord
func aoWebKitKeyChord(pageID C.uint64_t, key *C.char, ctrl, meta, alt, shift, held, composing C.int) C.int {
	page := webkitLookupPage(uint64(pageID))
	if page == nil || page.engine.events.KeyChord == nil {
		return 0
	}
	pressed := keybindings.Accelerator{Key: C.GoString(key), Ctrl: ctrl != 0, Meta: meta != 0, Alt: alt != 0, Shift: shift != 0}
	if page.engine.events.KeyChord(page.handle, pressed, keyPress{Held: held != 0, Composing: composing != 0}) {
		return 1
	}
	return 0
}

//export aoWebKitPageClosed
func aoWebKitPageClosed(pageID, popupID, profileID C.uint64_t) {
	if events, handle, ok := webkitViewOwner(uint64(pageID), uint64(popupID), uint64(profileID)); ok {
		events.PageClosed(handle)
	}
}

//export aoWebKitPageCrashed
func aoWebKitPageCrashed(pageID, popupID, profileID C.uint64_t, reason *C.char) {
	if events, handle, ok := webkitViewOwner(uint64(pageID), uint64(popupID), uint64(profileID)); ok {
		events.PageCrashed(handle, C.GoString(reason))
	}
}

// webkitViewOwner answers whom to tell what ended a view, on the GTK
// thread: its page's handle once the view answers to one, and before
// that, for a popup, the handle its number gives it, which its adoption
// keeps (pageHandle). The Manager records either at once (engineEvents).
func webkitViewOwner(pageID, popupID, profileID uint64) (engineEvents, ids.PageHandle, bool) {
	if page := webkitLookupPage(pageID); page != nil {
		return page.engine.events, page.handle, true
	}
	if popupID == 0 {
		return engineEvents{}, "", false
	}
	if profile := webkitLookupProfile(profileID); profile != nil {
		return profile.engine.events, pageHandle(popupID), true
	}
	return engineEvents{}, "", false
}

// aoWebKitPopup holds a popup WebKit created and answers its number, zero
// for one nothing may own. Its opener is the page openerID, or, while that
// is zero, the popup openerPopup, held or being adopted, whose handle is
// the one its adoption keeps (pageHandle).
//
//export aoWebKitPopup
func aoWebKitPopup(openerID, openerPopup, profileID C.uint64_t, view unsafe.Pointer, uri *C.char) C.uint64_t {
	target := webkitTakeString(uri)
	var opener ids.PageHandle
	if page := webkitLookupPage(uint64(openerID)); page != nil {
		opener = page.handle
	} else if openerPopup != 0 {
		opener = pageHandle(uint64(openerPopup))
	}
	profile := webkitLookupProfile(uint64(profileID))
	if opener == "" || profile == nil {
		// Nothing can own it, so nothing may keep it alive — but not YET, and
		// not from here. This export runs on the GTK thread inside `create`,
		// which is about to return this very widget to WebKit: unreffing it now
		// would hand WebKit a finalized object. And a close posted from here
		// would queue behind the delegate it is running inside, freezing the UI
		// for a full nativeCallTimeout. The goroutine's dispatch lands on a later GTK
		// turn, which is both safe and immediate.
		go webkitCloseView(view)
		return 0
	}
	n, handle := profile.engine.holdPopup(view, opener)
	// Before the view can report its close, which the popup's admission
	// then holds for its listing (engineEvents).
	profile.engine.events.PopupOpened(enginePopup{
		Profile: profile.Handle(), Opener: opener, Handle: handle, URL: target,
	})
	return C.uint64_t(n)
}

//export aoWebKitPagePresented
func aoWebKitPagePresented(pageID C.uint64_t) C.int {
	page := webkitLookupPage(uint64(pageID))
	if page == nil {
		return 0
	}
	// Already on the GTK thread inside a delegate; ask the host directly.
	return C.ao_wk_host_presented(page.view)
}

//export aoWebKitDownloadDestination
func aoWebKitDownloadDestination(profileID, pageID, popupID, downloadID C.uint64_t, download unsafe.Pointer, uri, suggested *C.char) {
	target, name := webkitTakeString(uri), webkitTakeString(suggested)
	profile := webkitLookupProfile(uint64(profileID))
	if profile == nil {
		C.ao_wk_download_cancel(download)
		C.ao_wk_download_unref(download)
		return
	}
	handle := downloadHandle(uint64(downloadID))
	if name == "" {
		name = string(handle)
	}
	pending := webkitPendingDownload{ptr: download, page: uint64(pageID), popup: uint64(popupID), handle: handle, url: target, name: name}
	if !profile.destinations.Hold(handle, pending) {
		// The profile is closing: Dispose cancels what it holds, and this
		// one goes the same way.
		C.ao_wk_download_decide(download, nil)
		C.ao_wk_download_unref(download)
		return
	}
	// Naming the staged file resolves the Downloads folder and prunes the
	// staging directory, file system work this GTK thread must not wait on;
	// the decision is completed on it afterwards, by whichever of its
	// worker and Dispose takes it first.
	go destination.Settle(uiThread, func() bool { return !uiThread.Alive() || profile.isDisposed() },
		func() destination.Staged { return stageFor(profile.stage, profile.releaseStaged, handle) },
		func(staged destination.Staged) bool {
			pending, ok := profile.destinations.Take(handle)
			if ok {
				profile.decideDownload(pending, staged.Path, staged.Err)
			}
			return ok
		})
}

// webkitCancelDestinations cancels and releases, on the GTK thread, the
// destination decisions p still holds, and refuses later ones (Dispose).
func webkitCancelDestinations(p *webkitProfile) {
	releaseDestinations.release(uiThread, func() {
		for _, pending := range p.destinations.Close() {
			C.ao_wk_download_decide(pending.ptr, nil)
			C.ao_wk_download_unref(pending.ptr)
		}
	})
}

// webkitPendingDownload is a download whose destination is being decided,
// with what its start report names.
type webkitPendingDownload struct {
	ptr         unsafe.Pointer
	page, popup uint64
	handle      downloadID
	url, name   string
}

// decideDownload completes download's destination decision on the GTK
// thread: it is written to the staged file at path, or canceled before a
// byte is written when stageErr says why there is none, the profile closed
// or the download failed meanwhile. Its start is reported here, so it is
// queued before anything WebKit reports of it.
func (p *webkitProfile) decideDownload(download webkitPendingDownload, path string, stageErr error) {
	start := p.downloadStart(webkitLookupPage(download.page), download.popup)
	start.ID, start.URL, start.SuggestedName = download.handle, download.url, download.name
	handle := download.handle
	if stageErr == nil && p.isDisposed() {
		// Its session goes on this thread after this turn (Dispose).
		stageErr = errStagedProfileClosed
	}
	var staged *C.char
	if stageErr == nil {
		staged = C.CString(path)
		defer C.free(unsafe.Pointer(staged))
	}
	switch {
	case C.ao_wk_download_decide(download.ptr, staged) == 0:
		if stageErr == nil {
			stageErr = errDownloadFailedUndecided
		}
	case stageErr == nil && !p.registerDownload(handle, download.ptr):
		C.ao_wk_download_cancel(download.ptr)
		stageErr = errStagedProfileClosed
	}
	if stageErr != nil {
		// Canceled before a byte was written; the row says why.
		p.engine.downloadEvents.Go(func() {
			if p.engine.events.DownloadStarted(start) == downloadAdmitted {
				p.engine.events.DownloadProgress(downloadProgress{ID: handle, State: downloadFailed, Error: stageErr.Error()})
			}
			webkitReleaseDownload(download.ptr)
		})
		return
	}
	p.engine.downloadEvents.Go(func() {
		if p.engine.events.DownloadStarted(start) != downloadAdmitted {
			if err := p.CancelDownload(handle); err != nil {
				log.Printf("browser: cancel refused download %s: %v", handle, err)
			}
		}
	})
}

//export aoWebKitDownloadProgress
func aoWebKitDownloadProgress(downloadID C.uint64_t, received, total C.double, state C.int, path, failure *C.char) {
	filePath, reason := webkitTakeString(path), webkitTakeString(failure)
	handle := downloadHandle(uint64(downloadID))
	// The profile that started the download owns the whole of its progress, so
	// a report arriving after that profile is disposed is a lookup miss rather
	// than an event routed at a torn-down workspace.
	profile := webkitDownloadOwner(handle)
	if profile == nil {
		return
	}
	progress := downloadProgress{ID: handle, Received: float64(received), Total: float64(total), State: downloadInProgress, FilePath: filePath, Error: reason}
	switch int(state) {
	case 1:
		progress.State = downloadCompleted
	case 2:
		progress.State = downloadCanceled
	case 3:
		progress.State = downloadFailed
	}
	// Off the GTK thread, after the start and in the order WebKit reported
	// them (downloadEvents).
	if progress.State != downloadInProgress {
		// Released before the event goes out: releasing dispatches BACK to the
		// GTK thread, which would queue behind the signal handler this export
		// is running inside and freeze the UI for a full nativeCallTimeout on
		// every finished download.
		profile.engine.downloadEvents.Go(func() {
			profile.releaseDownload(handle)
			profile.engine.events.DownloadProgress(progress)
		})
		return
	}
	profile.engine.downloadEvents.Go(func() { profile.engine.events.DownloadProgress(progress) })
}

func webkitLookupPage(id uint64) *webkitPage {
	if value, ok := webkitPageByID.Load(id); ok {
		return value.(*webkitPage)
	}
	return nil
}

func webkitLookupProfile(id uint64) *webkitProfile {
	if value, ok := webkitProfileByID.Load(id); ok {
		return value.(*webkitProfile)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The Go-typed surface the rest of the engine uses
// ---------------------------------------------------------------------------

// webkitAttachHost performs the one-time window surgery: the Wails SPA view
// becomes a GtkOverlay's main child and the 1x1 clipping background host is
// added beneath it. The SPA survives the reparent without a reload.
func webkitAttachHost(window unsafe.Pointer) error {
	ok := false
	// The glue copies both strings, and a withdrawn call never reads them.
	script, world := C.CString(webkitFindFrameScript), C.CString(webkitFindWorld)
	defer C.free(unsafe.Pointer(script))
	defer C.free(unsafe.Pointer(world))
	if uiThread.Run(func() {
		ok = C.ao_wk_host_attach(window) == 1
		C.ao_wk_set_find_frame_script(script, world)
	}) == uicall.Withdrawn {
		return errGTKUnavailable
	}
	if !ok {
		return fmt.Errorf("browser: the desktop window has no webview to host the browser pane beside")
	}
	return nil
}

// webkitParkView parks a new view in the host, or answers that the GTK
// thread did not take it in time, after which it never parks it.
func webkitParkView(view unsafe.Pointer) error {
	if uiThread.Run(func() { C.ao_wk_host_park(view) }) == uicall.Withdrawn {
		return errGTKUnavailable
	}
	return nil
}

// webkitPresentView shows the view at its placement: the fitted rect, its
// clip, and the scale the page is drawn at. The page's own size was set by
// SetViewport and is not touched here. A placement with no overlay pixel
// left to show hides the view, as webkitHideView does.
func webkitPresentView(view webkitView, placement PanePlacement) error {
	released := 0
	if err := view.do(func(view unsafe.Pointer) {
		var overlayW, overlayH C.int
		if C.ao_wk_overlay_size(&overlayW, &overlayH) == 0 {
			return
		}
		layout, scale, ok := overlayPaneLayout(placement, int(overlayW), int(overlayH))
		if !ok {
			released = int(C.ao_wk_host_hide(view))
			return
		}
		C.ao_wk_host_present(view, C.int(layout.Clip.Left), C.int(layout.Clip.Top),
			C.int(layout.Clip.Right), C.int(layout.Clip.Bottom),
			C.int(layout.OffsetX), C.int(layout.OffsetY), C.double(scale))
	}); err != nil {
		return err
	}
	return focusReleaseError(released)
}

// webkitSetViewBackground paints the page's base (pageBase) underneath the
// page: white under a document, the pane's color under a blank tab. color
// is packed 0xRRGGBB (nativePaneColor); GdkRGBA takes each channel in 0..1.
func webkitSetViewBackground(view webkitView, color uint32) error {
	red := float64((color>>16)&0xff) / 255
	green := float64((color>>8)&0xff) / 255
	blue := float64(color&0xff) / 255
	return view.do(func(view unsafe.Pointer) {
		C.ao_wk_view_set_background(view, C.double(red), C.double(green), C.double(blue))
	})
}

// webkitHideView parks the view. A view that held keyboard focus gives it to
// the SPA's webview; the error says when that handoff failed.
func webkitHideView(view webkitView) error {
	released := 0
	if err := view.do(func(view unsafe.Pointer) { released = int(C.ao_wk_host_hide(view)) }); err != nil {
		return err
	}
	return focusReleaseError(released)
}

// webkitNewSession makes profile profileID's session on the GTK thread. A
// session it made is the caller's to free (webkitFreeSession); a withdrawn
// call made none.
func webkitNewSession(dataDir, cacheDir, cookieFile string, ephemeral bool, profileID uint64) (unsafe.Pointer, error) {
	var session unsafe.Pointer
	flag := C.int(0)
	if ephemeral {
		flag = 1
	}
	if uiThread.Run(func() {
		cData, cCache := C.CString(dataDir), C.CString(cacheDir)
		cCookie := C.CString(cookieFile)
		session = C.ao_wk_session_new(cData, cCache, cCookie, flag, C.uint64_t(profileID))
		C.free(unsafe.Pointer(cData))
		C.free(unsafe.Pointer(cCache))
		C.free(unsafe.Pointer(cCookie))
	}) == uicall.Withdrawn {
		return nil, errGTKUnavailable
	}
	if session == nil {
		return nil, fmt.Errorf("browser: create workspace site-data session")
	}
	return session, nil
}

func webkitFreeSession(session unsafe.Pointer) {
	releaseSiteData.release(uiThread, func() { C.ao_wk_session_free(session) })
}

// webkitNewView builds a page's view in profile profileID's session on the
// GTK thread (withProfileOnUI): Dispose frees the session there after it
// unregisters the profile, so a session found there is not freed until the
// view holds it.
func webkitNewView(profileID, pageID uint64, userScript, consoleHandler string) (unsafe.Pointer, error) {
	var view unsafe.Pointer
	disposed := false
	// Withdrawn unless it ran: a view built after the caller gave up would
	// belong to no page.
	if uiThread.Run(func() {
		disposed = !withProfileOnUI(profileID, func(p *webkitProfile) {
			cScript, cHandler := C.CString(userScript), C.CString(consoleHandler)
			view = C.ao_wk_view_new(p.session, C.uint64_t(pageID), cScript, cHandler)
			C.free(unsafe.Pointer(cScript))
			C.free(unsafe.Pointer(cHandler))
		})
	}) == uicall.Withdrawn {
		return nil, errGTKUnavailable
	}
	if disposed {
		return nil, errProfileClosedBeforeView
	}
	if view == nil {
		return nil, fmt.Errorf("browser: create page view")
	}
	return view, nil
}

// withProfileOnUI runs fn with profile id on the GTK thread, where the
// caller runs it, unless the profile is unregistered or disposed, and
// answers whether fn ran. A caller carries the profile's id, never a
// native pointer it looked up elsewhere.
func withProfileOnUI(id uint64, fn func(p *webkitProfile)) bool {
	p := webkitLookupProfile(id)
	if p == nil || p.isDisposed() {
		return false
	}
	fn(p)
	return true
}

// webkitAdoptView makes a held popup's view page pageID's, or answers that
// the GTK thread did not take it in time, after which it never does: the
// caller closes the view.
func webkitAdoptView(view unsafe.Pointer, pageID uint64, userScript, consoleHandler string) error {
	if uiThread.Run(func() {
		cScript, cHandler := C.CString(userScript), C.CString(consoleHandler)
		C.ao_wk_view_adopt(view, C.uint64_t(pageID), cScript, cHandler)
		C.free(unsafe.Pointer(cScript))
		C.free(unsafe.Pointer(cHandler))
	}) == uicall.Withdrawn {
		return errGTKUnavailable
	}
	return nil
}

// webkitCloseView closes the view, giving keyboard focus it held to the SPA's
// webview. No caller can act on a failed handoff, so it is logged here.
func webkitCloseView(view unsafe.Pointer) {
	if err := webkitCloseViewErr(view); err != nil {
		log.Printf("browser: close a page: %v", err)
	}
}

// webkitCloseViewErr closes the view on the GTK thread and answers how its
// focus handoff ended. A close that timed out answers errGTKUnavailable,
// and still runs; its answer is read only from the closure that ran (a
// buffered completion).
func webkitCloseViewErr(view unsafe.Pointer) error {
	released := make(chan int, 1)
	if !uiThread.Release(func() { released <- int(C.ao_wk_view_close(view)) }) {
		return errGTKUnavailable
	}
	return focusReleaseError(<-released)
}

// webkitCloseViewSettled closes the view on the GTK thread and answers how
// its focus handoff ended once the close ran, however long the thread
// takes (uicall.Thread.Settle): errGTKUnavailable only when the loop went
// away first. A discard's receipt runs it (heldPopups.discard).
func webkitCloseViewSettled(view unsafe.Pointer) error {
	var code C.int
	if !uiThread.Settle(func() { code = C.ao_wk_view_close(view) }) {
		return errGTKUnavailable
	}
	return focusReleaseError(int(code))
}

func webkitOpenInspector(view webkitView) error {
	return view.do(func(view unsafe.Pointer) { C.ao_wk_view_open_inspector(view) })
}

func webkitSetViewSize(view webkitView, width, height int) error {
	return view.do(func(view unsafe.Pointer) { C.ao_wk_view_set_size(view, C.int(width), C.int(height)) })
}

func webkitLoadURI(view webkitView, uri string) error {
	return view.do(func(view unsafe.Pointer) {
		cURI := C.CString(uri)
		C.ao_wk_view_load_uri(view, cURI)
		C.free(unsafe.Pointer(cURI))
	})
}

// webkitHistory takes one history step; the C side numbers the steps as
// nativeHistoryStep does.
func webkitHistory(view webkitView, step nativeHistoryStep) error {
	return view.do(func(view unsafe.Pointer) { C.ao_wk_view_history(view, C.int(step)) })
}

func webkitCanGo(view webkitView, forward bool) (bool, error) {
	flag := C.int(0)
	if forward {
		flag = 1
	}
	answer := false
	err := view.do(func(view unsafe.Pointer) { answer = C.ao_wk_view_can_go(view, flag) == 1 })
	return answer, err
}

func webkitIsLoading(view webkitView) (bool, error) {
	loading := false
	err := view.do(func(view unsafe.Pointer) { loading = C.ao_wk_view_is_loading(view) == 1 })
	return loading, err
}

// gtkEvaluate runs one async-function body in the page and returns its JSON
// result: in the page's own script world, or in the isolated world named
// world. An undefined result is an empty answer, matching what CDP reports
// for a void expression.
func gtkEvaluate(ctx context.Context, view webkitView, world, body string) (string, error) {
	if !uiThread.Alive() {
		return "", errGTKUnavailable
	}
	id, ch := webkitRegisterCall()
	if err := view.do(func(view unsafe.Pointer) {
		cBody := C.CString(body)
		var cWorld *C.char
		if world != "" {
			cWorld = C.CString(world)
		}
		C.ao_wk_view_eval(view, cBody, cWorld, C.uint64_t(id))
		C.free(unsafe.Pointer(cBody))
		if cWorld != nil {
			C.free(unsafe.Pointer(cWorld))
		}
	}); err != nil {
		webkitCallByID.Delete(id)
		return "", err
	}
	select {
	case result := <-ch:
		if result.err != "" {
			return "", fmt.Errorf("%s", result.err)
		}
		return result.json, nil
	case <-ctx.Done():
		webkitCallByID.Delete(id)
		return "", ctx.Err()
	}
}

// webkitFind ops: a search from the start of the document, or a step.
const (
	webkitFindSearch   = C.AO_FIND_SEARCH
	webkitFindNext     = C.AO_FIND_NEXT
	webkitFindPrevious = C.AO_FIND_PREVIOUS
)

// webkitFindUnbounded is G_MAXUINT (guint is 32 bits), the count found-text
// reports past the search's max_match_count (WebKit-6.0.gir,
// WebKitFindController::found-text).
const webkitFindUnbounded = math.MaxUint32

// gtkFind runs one find on the view's WebKitFindController and waits for its
// found-text or failed-to-find-text answer.
func gtkFind(ctx context.Context, view webkitView, op int, text string, maxMatches uint32) (webkitCallResult, error) {
	id, ch := webkitRegisterCall()
	if err := view.do(func(view unsafe.Pointer) {
		cText := C.CString(text)
		C.ao_wk_view_find(view, C.int(op), cText, C.uint(maxMatches), C.uint64_t(id))
		C.free(unsafe.Pointer(cText))
	}); err != nil {
		webkitCallByID.Delete(id)
		return webkitCallResult{}, err
	}
	select {
	case result := <-ch:
		if result.found < 0 {
			return result, fmt.Errorf("browser: this page has no find controller")
		}
		return result, nil
	case <-ctx.Done():
		webkitCallByID.Delete(id)
		return webkitCallResult{}, ctx.Err()
	}
}

// escapeOffer is one Escape press handed to the key gate: with Control when
// Ctrl, followed by its release when Release.
type escapeOffer struct{ Ctrl, Release bool }

// offerEscapes hands the page's key gate the presses in order, inside one
// GTK callback so no other event (a focus change) lands between them, and
// answers which AO stopped (ao_wk_view_offer_escape). For the native smoke.
func (p *webkitPage) offerEscapes(offers []escapeOffer) ([]bool, error) {
	handled := make([]C.int, len(offers))
	err := p.liveView().do(func(view unsafe.Pointer) {
		for i, offer := range offers {
			handled[i] = C.ao_wk_view_offer_escape(view, flagInt(offer.Ctrl), flagInt(offer.Release))
		}
	})
	if err != nil {
		return nil, err
	}
	stopped := make([]bool, len(offers))
	for i, h := range handled {
		if h < 0 {
			return nil, fmt.Errorf("browser: page %d has no key controller", p.id)
		}
		stopped[i] = h == 1
	}
	return stopped, nil
}

// terminateWebProcess ends the page's web process, as a crash would
// (ao_wk_view_terminate_web_process). For the native smoke.
func (p *webkitPage) terminateWebProcess() error {
	return p.liveView().do(func(view unsafe.Pointer) { C.ao_wk_view_terminate_web_process(view) })
}

// onKeyPress has every KeyChord question of the page's engine reported to
// seen before it is answered, until the returned restore runs. It swaps the
// hook on the GTK thread, which is the only reader. For the native smoke.
func (p *webkitPage) onKeyPress(seen func(keybindings.Accelerator, keyPress)) (restore func() error, err error) {
	var orig func(ids.PageHandle, keybindings.Accelerator, keyPress) bool
	err = p.liveView().do(func(unsafe.Pointer) {
		orig = p.engine.events.KeyChord
		p.engine.events.KeyChord = func(handle ids.PageHandle, pressed keybindings.Accelerator, press keyPress) bool {
			seen(pressed, press)
			return orig(handle, pressed, press)
		}
	})
	if err != nil {
		return nil, err
	}
	return func() error {
		return p.liveView().do(func(unsafe.Pointer) { p.engine.events.KeyChord = orig })
	}, nil
}

// webkitFocusPresentedView gives a presented page's view keyboard focus
// while live holds. Both are checked on the GTK thread, where hides run,
// immediately before the grab.
func webkitFocusPresentedView(view webkitView, live func() bool) (focusResult, error) {
	result := C.int(0)
	withdrawn := false
	err := view.do(func(view unsafe.Pointer) {
		if !live() {
			withdrawn = true
			return
		}
		result = C.ao_wk_view_focus_presented(view)
	})
	switch {
	case err != nil:
		return focusNotPresented, err
	case withdrawn:
		return focusWithdrawn, nil
	case result < 0:
		return focusNotPresented, nil
	case result == 0:
		return focusNotPresented, fmt.Errorf("browser: the page's view did not take keyboard focus")
	}
	return focusMoved, nil
}

// webkitFocusApp hands keyboard focus to the SPA's webview.
func webkitFocusApp() error {
	ok := false
	if uiThread.Run(func() { ok = C.ao_wk_focus_app() == 1 }) == uicall.Withdrawn {
		return errGTKUnavailable
	}
	if !ok {
		return fmt.Errorf("browser: the desktop window has no app webview to focus")
	}
	return nil
}

// offerCtrlKey hands the page's key gate one Control press of key, as
// offerEscapes does, and answers whether AO stopped it. For the native
// smoke.
func (p *webkitPage) offerCtrlKey(key rune) (bool, error) {
	handled := C.int(0)
	err := p.liveView().do(func(view unsafe.Pointer) {
		handled = C.ao_wk_view_offer_ctrl_key(view, C.uint(key))
	})
	if err != nil {
		return false, err
	}
	if handled < 0 {
		return false, fmt.Errorf("browser: page %d has no key controller", p.id)
	}
	return handled == 1, nil
}

// webkitFocusHolder is which widget a page's window has as its focus
// widget (ao_wk_focus_holder). For the native smoke.
type webkitFocusHolder int

const (
	focusHeldByNone webkitFocusHolder = iota
	focusHeldByPage
	focusHeldByApp
	focusHeldByOther
)

// grabFocus gives the page's view keyboard focus, as a click into it
// does. For the native smoke.
func (p *webkitPage) grabFocus() error {
	return p.liveView().do(func(view unsafe.Pointer) { C.ao_wk_view_grab_focus(view) })
}

// presented answers whether a pane presents the page's view. For the native
// smoke.
func (p *webkitPage) presented() (bool, error) {
	on := C.int(0)
	err := p.liveView().do(func(view unsafe.Pointer) { on = C.ao_wk_host_presented(view) })
	return on == 1, err
}

// focusHolder answers who holds the page window's focus and whether that
// window is active. For the native smoke.
func (p *webkitPage) focusHolder() (webkitFocusHolder, bool, error) {
	holder, active := C.int(0), C.int(0)
	err := p.liveView().do(func(view unsafe.Pointer) {
		holder = C.ao_wk_focus_holder(view)
		active = C.ao_wk_window_active(view)
	})
	return webkitFocusHolder(holder), active == 1, err
}

// hasInputMethod answers whether the page's view has an input method
// context, the one the key gate reads a composition from. For the native
// smoke.
func (p *webkitPage) hasInputMethod() (bool, error) {
	has := C.int(0)
	err := p.liveView().do(func(view unsafe.Pointer) {
		has = C.ao_wk_view_has_input_method(view)
	})
	return has == 1, err
}

func flagInt(on bool) C.int {
	if on {
		return 1
	}
	return 0
}

// gtkFindFinish ends the view's find and clears its highlight.
func gtkFindFinish(view webkitView) error {
	return view.do(func(view unsafe.Pointer) { C.ao_wk_view_find_finish(view) })
}

// webkitSnapshot captures the view. It works on a parked view and returns
// fresh pixels after a DOM mutation, which is what makes hidden agent pages
// screenshot-able at all.
func webkitSnapshot(ctx context.Context, view webkitView, fullDocument bool) ([]byte, int, int, error) {
	id, ch := webkitRegisterCall()
	flag := C.int(0)
	if fullDocument {
		flag = 1
	}
	if err := view.do(func(view unsafe.Pointer) { C.ao_wk_view_snapshot(view, flag, C.uint64_t(id)) }); err != nil {
		webkitCallByID.Delete(id)
		return nil, 0, 0, err
	}
	select {
	case result := <-ch:
		if result.err != "" {
			return nil, 0, 0, fmt.Errorf("%s", result.err)
		}
		return result.pixels, result.width, result.height, nil
	case <-ctx.Done():
		webkitCallByID.Delete(id)
		return nil, 0, 0, ctx.Err()
	}
}

// webkitCancelDownload cancels p's download id on the GTK thread
// (withDownloadOnUI), where a released download's reference is dropped
// after it leaves p.downloads (releaseDownload), so the one it finds is
// still held through the cancel.
func webkitCancelDownload(p *webkitProfile, id downloadID) nativeCancel {
	found := false
	if uiThread.Run(func() {
		found = withDownloadOnUI(p, id, func(download unsafe.Pointer) { C.ao_wk_download_cancel(download) })
	}) == uicall.Withdrawn {
		return nativeCancelWithdrawn
	}
	if !found {
		return nativeCancelEnded
	}
	return nativeCanceled
}

// withDownloadOnUI runs fn with p's download id on the GTK thread, where
// the caller runs it, unless it was released, and answers whether fn ran.
func withDownloadOnUI(p *webkitProfile, id downloadID, fn func(download unsafe.Pointer)) bool {
	download := p.heldDownload(id)
	if download == nil {
		return false
	}
	fn(download)
	return true
}

func webkitReleaseDownload(download unsafe.Pointer) {
	releaseDownloadRef.release(uiThread, func() { C.ao_wk_download_unref(download) })
}

// webkitDownloadCounts is how many downloads WebKit started and how many it
// finalized since the process began; each only grows. Their difference is
// how many are alive: one the engine still holds, or WebKit does.
func webkitDownloadCounts() (started, freed int) {
	var s, f C.int
	C.ao_wk_download_counts(&s, &f)
	return int(s), int(f)
}

// webkitDownloadsAlive is how many downloads WebKit made are not finalized
// yet (webkitDownloadCounts).
func webkitDownloadsAlive() int {
	started, freed := webkitDownloadCounts()
	return started - freed
}
