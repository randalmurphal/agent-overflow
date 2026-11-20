//go:build darwin && cgo && !ios && !server && !nogui

package browser

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit

#include <stdlib.h>
#include "wkwebviewglue_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"agent-overflow/internal/browser/destination"
	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/uicall"
	"agent-overflow/internal/keybindings"
)

// The only cgo in the WKWebView engine. Everything above it — the engine, the
// profile, the page driver — is ordinary Go against the small Go-typed surface
// declared here, so exactly one file has to be read with Objective-C in mind.
//
// Two rules this file exists to enforce, the same two the WebKitGTK engine's
// cgo file enforces:
//
//  1. EVERY WebKit or AppKit call happens on the main thread, through
//     `uiThread`: Run for an operation, Release for giving up what is held.
//     Cocoa owns that thread; calling UIKit-class APIs from a Go goroutine is
//     undefined behaviour, not a race that shows up under load.
//  2. No Go pointer is ever handed to C. Callbacks address a page, profile,
//     call, or download by a uint64 id and this file resolves it, which is also
//     what makes a callback arriving after teardown a lookup miss rather than a
//     crash.

// wkClearTimeout bounds the whole site-data clear. nativeCallTimeout bounds
// ONE main-thread dispatch; this covers WebKit's own asynchronous removal of
// every store it holds — one round trip per workspace the user has ever
// opened — so it is deliberately longer. The Settings button must fail rather
// than park its caller forever on a WebKit callback that never arrives.
const wkClearTimeout = 30 * time.Second

var errWKUnavailable = fmt.Errorf("browser: the desktop window is not accepting browser work")

// errProfileClosedBeforeView is a new page's answer when its profile was
// disposed before its view was built (wkNewView).
var errProfileClosedBeforeView = errors.New("browser: workspace profile closed before the page's view was built")

// wkView is a live page's view as the page's operations reach it. The page's
// lifetime ends (wkPage.discard) before Close queues the view's release, and
// the main thread runs dispatches in order, so a dispatch that still finds
// the lifetime live there runs before the release.
type wkView struct {
	ptr  unsafe.Pointer
	life context.Context
}

// do runs fn with the view on the main thread, unless the page closed first
// or the thread did not take it in time, after which fn never runs.
func (v wkView) do(fn func(view unsafe.Pointer)) error {
	closed := false
	if uiThread.Run(func() {
		if v.life.Err() != nil {
			closed = true
			return
		}
		fn(v.ptr)
	}) == uicall.Withdrawn {
		return errWKUnavailable
	}
	if closed {
		return errViewClosed
	}
	return nil
}

// wkOnMainThread answers whether a closure posted to uiThread would run
// inline right now: true only on the AppKit main thread, where Wails'
// dispatch short-circuits.
func wkOnMainThread() bool { return C.ao_wkv_on_main_thread() == 1 }

// ---------------------------------------------------------------------------
// Registries the Objective-C callbacks resolve against
// ---------------------------------------------------------------------------

var (
	wkPageSeq     atomic.Uint64
	wkProfileSeq  atomic.Uint64
	wkCallSeq     atomic.Uint64
	wkPageByID    sync.Map // uint64 -> *wkPage
	wkProfileByID sync.Map // uint64 -> *wkProfile
	wkCallByID    sync.Map // uint64 -> chan wkCallResult
)

// wkCallResult is one completed asynchronous WebKit call. Only one of the two
// shapes is populated, decided by which call was issued.
type wkCallResult struct {
	json   string
	err    string
	pixels []byte
	width  int
	height int
	// found is a find's answer: 1 found, 0 none, -1 unusable.
	found int
}

func wkRegisterCall() (uint64, chan wkCallResult) {
	id := wkCallSeq.Add(1)
	ch := make(chan wkCallResult, 1)
	wkCallByID.Store(id, ch)
	return id, ch
}

func wkCompleteCall(id uint64, result wkCallResult) {
	value, ok := wkCallByID.LoadAndDelete(id)
	if !ok {
		return
	}
	// Buffered by one and delivered once: a caller that already gave up on its
	// deadline leaves nothing blocked here.
	value.(chan wkCallResult) <- result
}

// wkTakeString copies a malloc'd string and releases it. Every char* the
// Objective-C half hands over is owned by the Go half from that point on.
func wkTakeString(text *C.char) string {
	if text == nil {
		return ""
	}
	value := C.GoString(text)
	C.ao_wkv_free(unsafe.Pointer(text))
	return value
}

//export aoWKVEvalDone
func aoWKVEvalDone(callID C.uint64_t, jsonText *C.char, errText *C.char) {
	wkCompleteCall(uint64(callID), wkCallResult{
		json: wkTakeString(jsonText), err: wkTakeString(errText),
	})
}

//export aoWKVFindDone
func aoWKVFindDone(callID C.uint64_t, found C.int) {
	wkCompleteCall(uint64(callID), wkCallResult{found: int(found)})
}

//export aoWKVClearDone
func aoWKVClearDone(callID C.uint64_t, errText *C.char) {
	// Lands on the main queue, like every other export here, and does nothing
	// but the registry lookup and one buffered send — never a uiThread call, which would
	// queue behind the very main-thread turn this is running on.
	wkCompleteCall(uint64(callID), wkCallResult{err: wkTakeString(errText)})
}

//export aoWKVSnapshotDone
func aoWKVSnapshotDone(callID C.uint64_t, pixels unsafe.Pointer, width, height, stride C.int, errText *C.char) {
	result := wkCallResult{width: int(width), height: int(height), err: wkTakeString(errText)}
	if pixels != nil {
		result.pixels = C.GoBytes(pixels, C.int(int(stride)*int(height)))
		C.ao_wkv_free(pixels)
	}
	wkCompleteCall(uint64(callID), result)
}

//export aoWKVAllow
func aoWKVAllow(pageID, _ C.uint64_t, decision unsafe.Pointer, uri *C.char, download C.int) {
	target := wkTakeString(uri)
	page := uint64(pageID)
	// Answered OFF the main thread: navigation authority is the Manager's, and
	// asking it takes Manager locks — blocking the main thread on a Go lock is
	// how the whole window freezes behind one browser operation. The delegate
	// deferred the decision with a copied block held for exactly this.
	go func() {
		verdict := C.int(C.AO_POLICY_CANCEL)
		if wkNavigationAllowed(page, target) {
			// The Manager's authority is over the URL; whether an allowed URL
			// navigates or downloads is what the anchor asked for.
			verdict = C.AO_POLICY_ALLOW
			if download != 0 {
				verdict = C.AO_POLICY_DOWNLOAD
			}
		}
		// The decision's block is given up with its answer. A loop that is
		// gone takes the page with it, so an unanswered decision dies with the
		// process rather than blocking anything.
		releasePolicyAnswer.release(uiThread, func() { C.ao_wkv_policy_finish(decision, verdict) })
	}()
}

// wkNavigationAllowed answers one navigation in a view with the policy of
// the page the glue names for it (ao_view_policy_page_id): the view's own
// page, or for a popup the Manager has not adopted yet, which loads from
// creation, its opener's. The opener's workspace authority is the one the
// adopted popup gets, so the file policy is the same before and after
// adoption, whatever cookie jar the pages are in. A page that is gone, or a
// view with none, refuses.
func wkNavigationAllowed(pageID uint64, target string) bool {
	page := wkLookupPage(pageID)
	return page != nil && (page.hooks.Allow == nil || page.hooks.Allow(target))
}

//export aoWKVConsole
func aoWKVConsole(pageID C.uint64_t, payload *C.char) {
	text := wkTakeString(payload)
	if page := wkLookupPage(uint64(pageID)); page != nil {
		page.consoleMessage(text)
	}
}

//export aoWKVPageInfo
func aoWKVPageInfo(pageID C.uint64_t, uri *C.char, title *C.char) {
	location, name := wkTakeString(uri), wkTakeString(title)
	page := wkLookupPage(uint64(pageID))
	if page == nil {
		return
	}
	page.noteLoad()
	if page.hooks.Info == nil {
		return
	}
	// Numbered here, on the main thread, in the order the view changed; the
	// Manager takes its own locks, so the report leaves the thread.
	order := page.infoOrder.Add(1)
	go page.hooks.Info(location, name, order)
}

//export aoWKVPageLoading
func aoWKVPageLoading(pageID C.uint64_t, loading C.int) {
	page := wkLookupPage(uint64(pageID))
	if page == nil || page.hooks.Loading == nil {
		return
	}
	// Numbered here, on the main thread, in the order the view changed; the
	// Manager takes its own locks, so the report leaves the thread.
	order := page.loadOrder.Add(1)
	go page.hooks.Loading(loading != 0, order)
}

//export aoWKVDocumentCommitted
func aoWKVDocumentCommitted(pageID C.uint64_t) {
	page := wkLookupPage(uint64(pageID))
	if page == nil || page.hooks.DocumentCommitted == nil {
		return
	}
	// The commit's order is stamped here, on the main thread, before any
	// later page operation can be dispatched; finish only submits the find's
	// end, which the Manager applies off this thread.
	if finish := page.hooks.DocumentCommitted(); finish != nil {
		finish()
	}
}

// aoWKVKeyChord runs ON the main thread, inside the key event's delivery, and
// answers synchronously: it is a set lookup in the Manager and nothing that
// waits. The routing that follows a claim leaves the thread on a goroutine.
//
//export aoWKVKeyChord
func aoWKVKeyChord(pageID C.uint64_t, key *C.char, ctrl, meta, alt, shift, repeat, composing C.int) C.int {
	page := wkLookupPage(uint64(pageID))
	if page == nil || page.engine.events.KeyChord == nil {
		return 0
	}
	pressed := keybindings.Accelerator{Key: C.GoString(key), Ctrl: ctrl != 0, Meta: meta != 0, Alt: alt != 0, Shift: shift != 0}
	if page.engine.events.KeyChord(page.handle, pressed, keyPress{Repeat: repeat != 0, Composing: composing != 0}) {
		return 1
	}
	return 0
}

//export aoWKVPageClosed
func aoWKVPageClosed(pageID, popupID, profileID C.uint64_t) {
	if events, handle, ok := wkViewOwner(uint64(pageID), uint64(popupID), uint64(profileID)); ok {
		events.PageClosed(handle)
	}
}

//export aoWKVPageCrashed
func aoWKVPageCrashed(pageID, popupID, profileID C.uint64_t) {
	if events, handle, ok := wkViewOwner(uint64(pageID), uint64(popupID), uint64(profileID)); ok {
		events.PageCrashed(handle, "web content process terminated")
	}
}

// wkViewOwner answers whom to tell what ended a view, on the main thread:
// its page's handle once the view answers to one, and before that, for a
// popup, the handle its number gives it, which its adoption keeps
// (pageHandle). The Manager records either at once (engineEvents).
func wkViewOwner(pageID, popupID, profileID uint64) (engineEvents, ids.PageHandle, bool) {
	if page := wkLookupPage(pageID); page != nil {
		return page.engine.events, page.handle, true
	}
	if popupID == 0 {
		return engineEvents{}, "", false
	}
	if profile := wkLookupProfile(profileID); profile != nil {
		return profile.engine.events, pageHandle(popupID), true
	}
	return engineEvents{}, "", false
}

// aoWKVPopup holds a popup WebKit created and answers its number, zero for
// one nothing may own. Its opener is the page openerID, or, while that is
// zero, the popup openerPopup, held or being adopted, whose handle is the
// one its adoption keeps (pageHandle).
//
//export aoWKVPopup
func aoWKVPopup(openerID, openerPopup, profileID C.uint64_t, view unsafe.Pointer, uri *C.char) C.uint64_t {
	target := wkTakeString(uri)
	var opener ids.PageHandle
	if page := wkLookupPage(uint64(openerID)); page != nil {
		opener = page.handle
	} else if openerPopup != 0 {
		opener = pageHandle(uint64(openerPopup))
	}
	profile := wkLookupProfile(uint64(profileID))
	if opener == "" || profile == nil {
		// Nothing can own it, so nothing may keep it alive — but not YET. This
		// export runs inside -createWebViewWithConfiguration:, which is about to
		// return this very view to WebKit; releasing it here would hand WebKit a
		// dead object. The goroutine's dispatch lands on a later main-thread
		// turn, after the delegate has returned.
		go wkCloseView(view)
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

//export aoWKVPagePresented
func aoWKVPagePresented(pageID C.uint64_t) C.int {
	page := wkLookupPage(uint64(pageID))
	if page == nil {
		return 0
	}
	// Already on the main thread inside a delegate; ask the host directly.
	return C.ao_wkv_host_presented(page.view)
}

//export aoWKVDownloadDestination
func aoWKVDownloadDestination(profileID, pageID, popupID, downloadID C.uint64_t, download unsafe.Pointer, uri, suggested *C.char) {
	target, name := wkTakeString(uri), wkTakeString(suggested)
	profile := wkLookupProfile(uint64(profileID))
	if profile == nil {
		C.ao_wkv_download_decide(download, nil)
		C.ao_wkv_download_release(download)
		return
	}
	handle := downloadHandle(uint64(downloadID))
	if name == "" {
		name = string(handle)
	}
	pending := wkPendingDownload{ptr: download, page: uint64(pageID), popup: uint64(popupID), handle: handle, url: target, name: name}
	if !profile.destinations.Hold(handle, pending) {
		// The profile is closing, and Dispose cancels what it holds, or the
		// id already has an open decision, which stays: this one is canceled.
		C.ao_wkv_download_decide(download, nil)
		C.ao_wkv_download_release(download)
		return
	}
	// Naming the staged file resolves the Downloads folder and prunes the
	// staging directory, file system work the main thread must not wait on;
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

// wkCancelDestinations cancels and releases, on the main thread, the
// destination decisions p still holds, and refuses later ones (Dispose).
func wkCancelDestinations(p *wkProfile) {
	releaseDestinations.release(uiThread, func() {
		for _, pending := range p.destinations.Close() {
			C.ao_wkv_download_decide(pending.ptr, nil)
			C.ao_wkv_download_release(pending.ptr)
		}
	})
}

// wkPendingDownload is a download whose destination is being decided, with
// what its start report names.
type wkPendingDownload struct {
	ptr         unsafe.Pointer
	page, popup uint64
	handle      downloadID
	url, name   string
}

// decideDownload completes download's destination decision on the main
// thread: it is written to the staged file at path, or canceled before a
// byte is written when stageErr says why there is none, the profile closed
// or the download failed meanwhile. Its start is reported here, so it is
// queued before anything WebKit reports of it.
func (p *wkProfile) decideDownload(download wkPendingDownload, path string, stageErr error) {
	start := p.downloadStart(wkLookupPage(download.page), download.popup)
	start.ID, start.URL, start.SuggestedName = download.handle, download.url, download.name
	start.EndReported = p.ends.endReported(download.handle)
	handle := download.handle
	if stageErr == nil && p.isDisposed() {
		stageErr = errStagedProfileClosed
	}
	var staged *C.char
	if stageErr == nil {
		staged = C.CString(path)
		defer C.free(unsafe.Pointer(staged))
	}
	switch {
	case C.ao_wkv_download_decide(download.ptr, staged) == 0:
		if stageErr == nil {
			stageErr = errDownloadFailedUndecided
		}
	case stageErr == nil && !p.registerDownload(handle, download.ptr):
		C.ao_wkv_download_cancel(download.ptr)
		stageErr = errStagedProfileClosed
	}
	if stageErr != nil {
		// Canceled before a byte was written; the row says why.
		p.engine.downloadEvents.Go(func() {
			if p.engine.events.DownloadStarted(start) == downloadAdmitted {
				p.engine.events.DownloadProgress(downloadProgress{ID: handle, State: downloadFailed, Error: stageErr.Error()})
			}
			wkReleaseDownload(download.ptr)
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

//export aoWKVDownloadFinished
func aoWKVDownloadFinished(downloadID C.uint64_t, received C.double, state C.int, path, failure *C.char) {
	filePath, reason := wkTakeString(path), wkTakeString(failure)
	handle := downloadHandle(uint64(downloadID))
	// The profile that started the download owns the whole of its progress, so a
	// report arriving after that profile is disposed is a lookup miss rather
	// than an event routed at a torn-down workspace.
	profile := wkDownloadOwner(handle)
	if profile == nil {
		return
	}
	progress := downloadProgress{ID: handle, Received: float64(received), State: downloadCompleted, FilePath: filePath, Error: reason}
	switch int(state) {
	case 2:
		progress.State = downloadCanceled
	case 3:
		progress.State = downloadFailed
	}
	// The end is the Manager's to settle from here, also for a page that
	// leaves before it is delivered (downloadStart.EndReported).
	profile.ends.received(handle)
	// Both halves go off the main thread, after the start (downloadEvents):
	// releasing the download dispatches BACK to the main thread, which would
	// queue behind the delegate this export is running inside, and routing
	// the event takes Manager locks.
	profile.engine.downloadEvents.Go(func() {
		defer profile.ends.reported(handle)
		profile.releaseDownload(handle)
		profile.engine.events.DownloadProgress(progress)
	})
}

func wkLookupPage(id uint64) *wkPage {
	if value, ok := wkPageByID.Load(id); ok {
		return value.(*wkPage)
	}
	return nil
}

func wkLookupProfile(id uint64) *wkProfile {
	if value, ok := wkProfileByID.Load(id); ok {
		return value.(*wkProfile)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The Go-typed surface the rest of the engine uses
// ---------------------------------------------------------------------------

// wkFocusPresentedView makes a presented page's view its window's first
// responder while live holds. Both are checked on the main thread, where
// hides run, immediately before the change.
func wkFocusPresentedView(view wkView, live func() bool) (focusResult, error) {
	result := C.int(0)
	withdrawn := false
	err := view.do(func(view unsafe.Pointer) {
		if !live() {
			withdrawn = true
			return
		}
		result = C.ao_wkv_view_focus_presented(view)
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

// wkFocusApp hands keyboard focus to the SPA's webview.
func wkFocusApp() error {
	ok := false
	if uiThread.Run(func() { ok = C.ao_wkv_focus_app() == 1 }) == uicall.Withdrawn {
		return errWKUnavailable
	}
	if !ok {
		return fmt.Errorf("browser: the desktop window's app webview did not take keyboard focus")
	}
	return nil
}

// wkAttachHost adds AO's clipping park view beneath the Wails window's own
// subviews. The SPA webview is untouched: unlike GTK4, AppKit lets a second
// view join an existing content view without any reparenting surgery.
func wkAttachHost(window unsafe.Pointer) error {
	ok := false
	// The glue copies both strings, and a withdrawn call never reads them.
	script, world := C.CString(webkitFindFrameScript), C.CString(webkitFindWorld)
	defer C.free(unsafe.Pointer(script))
	defer C.free(unsafe.Pointer(world))
	if uiThread.Run(func() {
		ok = C.ao_wkv_host_attach(window) == 1
		C.ao_wkv_set_find_frame_script(script, world)
	}) == uicall.Withdrawn {
		return errWKUnavailable
	}
	if !ok {
		return fmt.Errorf("browser: the desktop window has no content view to host the browser pane in")
	}
	return nil
}

// wkParkView parks a new view in its slot, or answers that the main thread
// did not take it in time, after which it never parks it.
func wkParkView(view unsafe.Pointer, slot int) error {
	if uiThread.Run(func() { C.ao_wkv_host_park(view, C.int(slot)) }) == uicall.Withdrawn {
		return errWKUnavailable
	}
	return nil
}

// wkPresentView moves the view over the placement's fitted rect, cropped to
// its visible intersection. The page's viewport was set by SetViewport and
// stays its bounds, so the fitted frame over those bounds is the scale. A
// placement with no backing pixel left to show hides the view instead, parked
// in slot as wkHideView parks it.
func wkPresentView(view wkView, slot int, placement PanePlacement) error {
	rect := placement.Rect
	released := 0
	if err := view.do(func(view unsafe.Pointer) {
		released = int(C.ao_wkv_host_present(view, C.int(slot), C.double(rect.X), C.double(rect.Y),
			C.double(rect.Width), C.double(rect.Height),
			C.double(rect.ClipX), C.double(rect.ClipY),
			C.double(rect.ClipWidth), C.double(rect.ClipHeight),
			C.double(rect.ViewportWidth), C.double(rect.ViewportHeight)))
	}); err != nil {
		return err
	}
	return focusReleaseError(released)
}

// wkSetViewBackground paints the page's base (pageBase), packed 0xRRGGBB,
// under the page. The colour is parsed before the dispatch: the Objective-C
// half takes the packed value and never a string.
func wkSetViewBackground(view wkView, code int) error {
	return view.do(func(view unsafe.Pointer) {
		C.ao_wkv_view_set_background(view, C.int(code))
	})
}

// wkHideView parks the view. A view that held keyboard focus gives it to the
// SPA's webview; the error says when that handoff failed.
func wkHideView(view wkView, slot int) error {
	released := 0
	if err := view.do(func(view unsafe.Pointer) { released = int(C.ao_wkv_host_hide(view, C.int(slot))) }); err != nil {
		return err
	}
	return focusReleaseError(released)
}

// wkNewStore makes a profile's store on the main thread. A store it made is
// the caller's to free (wkFreeStore); a withdrawn call made none.
func wkNewStore(identifier string, ephemeral bool) (unsafe.Pointer, error) {
	var store unsafe.Pointer
	flag := C.int(0)
	if ephemeral {
		flag = 1
	}
	if uiThread.Run(func() {
		cID := C.CString(identifier)
		store = C.ao_wkv_store_new(cID, flag)
		C.free(unsafe.Pointer(cID))
	}) == uicall.Withdrawn {
		return nil, errWKUnavailable
	}
	if store == nil {
		return nil, fmt.Errorf("browser: create workspace site-data store")
	}
	return store, nil
}

func wkFreeStore(store unsafe.Pointer) {
	releaseSiteData.release(uiThread, func() { C.ao_wkv_store_free(store) })
}

// wkClearSiteData removes every WKWebsiteDataStore this app has created, which
// IS this engine's whole persisted site data: only +dataStoreForIdentifier:
// stores are enumerable, every one of them is a workspace store AO asked for
// inside this app's container, and the SPA webview's default store has no
// identifier and is never returned. There is no directory to delete beside it —
// WebKit owns where those stores live, which is exactly why this engine has to
// implement the capability at all.
//
// It deliberately does NOT need a started engine: removal is class-level WebKit
// API with no view, no store object, and no host in it, and the Settings button
// must work on a Mac that has not opened a browser page this run. What it does
// need is the app loop uiThread posts to — always there in the desktop app,
// and a build without one has no engine to reach this through.
func wkClearSiteData(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, wkClearTimeout)
	defer cancel()
	if !uiThread.Alive() {
		return errWKUnavailable
	}
	id, ch := wkRegisterCall()
	if uiThread.Run(func() { C.ao_wkv_clear_data(C.uint64_t(id)) }) == uicall.Withdrawn {
		wkCallByID.Delete(id)
		return errWKUnavailable
	}
	select {
	case result := <-ch:
		return wkClearSiteDataFailure(result.err)
	case <-ctx.Done():
		wkCallByID.Delete(id)
		return ctx.Err()
	}
}

// wkNewView builds a page's view in profile profileID's store on the main
// thread (withProfileOnUI): Dispose frees the store there after it
// unregisters the profile, so a store found there is not freed until the
// view holds it.
func wkNewView(pageID, profileID uint64, userScript, consoleHandler string) (unsafe.Pointer, error) {
	var view unsafe.Pointer
	disposed := false
	// Withdrawn unless it ran: a view built after the caller gave up would
	// belong to no page.
	if uiThread.Run(func() {
		disposed = !withProfileOnUI(profileID, func(p *wkProfile) {
			cScript, cHandler := C.CString(userScript), C.CString(consoleHandler)
			view = C.ao_wkv_view_new(p.store, C.uint64_t(pageID), C.uint64_t(profileID), cScript, cHandler)
			C.free(unsafe.Pointer(cScript))
			C.free(unsafe.Pointer(cHandler))
		})
	}) == uicall.Withdrawn {
		return nil, errWKUnavailable
	}
	if disposed {
		return nil, errProfileClosedBeforeView
	}
	if view == nil {
		return nil, fmt.Errorf("browser: create page view")
	}
	return view, nil
}

// withProfileOnUI runs fn with profile id on the main thread, where the
// caller runs it, unless the profile is unregistered or disposed, and
// answers whether fn ran. A caller carries the profile's id, never a
// native pointer it looked up elsewhere.
func withProfileOnUI(id uint64, fn func(p *wkProfile)) bool {
	p := wkLookupProfile(id)
	if p == nil || p.isDisposed() {
		return false
	}
	fn(p)
	return true
}

// wkAdoptView makes a held popup's view page pageID's, or answers that the
// main thread did not take it in time, after which it never does: the
// caller closes the view.
func wkAdoptView(view unsafe.Pointer, pageID, profileID uint64, userScript, consoleHandler string) error {
	if uiThread.Run(func() {
		cScript, cHandler := C.CString(userScript), C.CString(consoleHandler)
		C.ao_wkv_view_adopt(view, C.uint64_t(pageID), C.uint64_t(profileID), cScript, cHandler)
		C.free(unsafe.Pointer(cScript))
		C.free(unsafe.Pointer(cHandler))
	}) == uicall.Withdrawn {
		return errWKUnavailable
	}
	return nil
}

// wkCloseView closes the view, giving keyboard focus it held to the SPA's
// webview. No caller can act on a failed handoff, so it is logged here.
func wkCloseView(view unsafe.Pointer) {
	if err := wkCloseViewErr(view); err != nil {
		log.Printf("browser: close a page: %v", err)
	}
}

// wkCloseViewErr closes the view on the main thread and answers how its
// focus handoff ended. A close that timed out answers errWKUnavailable, and
// still runs; its answer is read only from the closure that ran (a
// buffered completion).
func wkCloseViewErr(view unsafe.Pointer) error {
	released := make(chan int, 1)
	if !uiThread.Release(func() { released <- int(C.ao_wkv_view_close(view)) }) {
		return errWKUnavailable
	}
	return focusReleaseError(<-released)
}

// wkCloseViewSettled closes the view on the main thread and answers how
// its focus handoff ended once the close ran, however long the thread
// takes (uicall.Thread.Settle): errWKUnavailable only when the loop went
// away first. A discard's receipt runs it (heldPopups.discard).
func wkCloseViewSettled(view unsafe.Pointer) error {
	var code C.int
	if !uiThread.Settle(func() { code = C.ao_wkv_view_close(view) }) {
		return errWKUnavailable
	}
	return focusReleaseError(int(code))
}

func wkSetViewSize(view wkView, width, height int) error {
	return view.do(func(view unsafe.Pointer) { C.ao_wkv_view_set_size(view, C.int(width), C.int(height)) })
}

func wkViewSize(view wkView) (int, int, error) {
	var width, height C.int
	if err := view.do(func(view unsafe.Pointer) { C.ao_wkv_view_get_size(view, &width, &height) }); err != nil {
		return 0, 0, err
	}
	return int(width), int(height), nil
}

func wkLoadURI(view wkView, uri string) error {
	return view.do(func(view unsafe.Pointer) {
		cURI := C.CString(uri)
		C.ao_wkv_view_load_uri(view, cURI)
		C.free(unsafe.Pointer(cURI))
	})
}

// wkLoadFile is the file:// path. WKWebView refuses a file URL handed to
// -loadRequest:; -loadFileURL:allowingReadAccessToURL: is the documented way,
// and the read-access root is what lets the page reach its own siblings.
func wkLoadFile(view wkView, path, readAccessDir string) error {
	return view.do(func(view unsafe.Pointer) {
		cPath, cRoot := C.CString(path), C.CString(readAccessDir)
		C.ao_wkv_view_load_file(view, cPath, cRoot)
		C.free(unsafe.Pointer(cPath))
		C.free(unsafe.Pointer(cRoot))
	})
}

// wkHistory takes one history step; the Objective-C side numbers the steps
// as nativeHistoryStep does.
func wkHistory(view wkView, step nativeHistoryStep) error {
	return view.do(func(view unsafe.Pointer) { C.ao_wkv_view_history(view, C.int(step)) })
}

func wkCanGo(view wkView, forward bool) (bool, error) {
	flag := C.int(0)
	if forward {
		flag = 1
	}
	answer := false
	err := view.do(func(view unsafe.Pointer) { answer = C.ao_wkv_view_can_go(view, flag) == 1 })
	return answer, err
}

func wkIsLoading(view wkView) (bool, error) {
	loading := false
	err := view.do(func(view unsafe.Pointer) { loading = C.ao_wkv_view_is_loading(view) == 1 })
	return loading, err
}

// wkFind runs one -findString:withConfiguration:completionHandler: and
// reports whether it found a match.
func wkFind(ctx context.Context, view wkView, text string, backwards bool) (bool, error) {
	if !uiThread.Alive() {
		return false, errWKUnavailable
	}
	id, ch := wkRegisterCall()
	flag := C.int(0)
	if backwards {
		flag = 1
	}
	if err := view.do(func(view unsafe.Pointer) {
		cText := C.CString(text)
		C.ao_wkv_view_find(view, cText, flag, C.uint64_t(id))
		C.free(unsafe.Pointer(cText))
	}); err != nil {
		wkCallByID.Delete(id)
		return false, err
	}
	select {
	case result := <-ch:
		if result.found < 0 {
			return false, fmt.Errorf("browser: the page cannot search for this text")
		}
		return result.found == 1, nil
	case <-ctx.Done():
		wkCallByID.Delete(id)
		return false, ctx.Err()
	}
}

// wkEvaluate runs one async-function body in the page and returns its JSON
// result: in the page's content world, or in the isolated one named world. An
// undefined result is an empty answer, matching what CDP reports for a void
// expression.
func wkEvaluate(ctx context.Context, view wkView, world, body string) (string, error) {
	if !uiThread.Alive() {
		return "", errWKUnavailable
	}
	id, ch := wkRegisterCall()
	if err := view.do(func(view unsafe.Pointer) {
		cBody := C.CString(body)
		var cWorld *C.char
		if world != "" {
			cWorld = C.CString(world)
		}
		C.ao_wkv_view_eval(view, cBody, cWorld, C.uint64_t(id))
		C.free(unsafe.Pointer(cBody))
		if cWorld != nil {
			C.free(unsafe.Pointer(cWorld))
		}
	}); err != nil {
		wkCallByID.Delete(id)
		return "", err
	}
	select {
	case result := <-ch:
		if result.err != "" {
			return "", fmt.Errorf("%s", result.err)
		}
		return result.json, nil
	case <-ctx.Done():
		wkCallByID.Delete(id)
		return "", ctx.Err()
	}
}

// wkSnapshot captures the view at its CURRENT size. WKSnapshotConfiguration
// cannot reach past the view's bounds, so a full-document capture is the page
// driver resizing the view first — which is why this call has no region flag,
// unlike the GTK one.
func wkSnapshot(ctx context.Context, view wkView) ([]byte, int, int, error) {
	id, ch := wkRegisterCall()
	if err := view.do(func(view unsafe.Pointer) { C.ao_wkv_view_snapshot(view, C.uint64_t(id)) }); err != nil {
		wkCallByID.Delete(id)
		return nil, 0, 0, err
	}
	select {
	case result := <-ch:
		if result.err != "" {
			return nil, 0, 0, fmt.Errorf("%s", result.err)
		}
		return result.pixels, result.width, result.height, nil
	case <-ctx.Done():
		wkCallByID.Delete(id)
		return nil, 0, 0, ctx.Err()
	}
}

// wkCancelDownload cancels p's download id on the main thread
// (withDownloadOnUI), where a released download is let go after it leaves
// p.downloads (releaseDownload), so the one it finds is still held through
// the cancel. One whose end WebKit reported there (p.ends) has ended, its
// release still queued, and is not canceled.
func wkCancelDownload(p *wkProfile, id downloadID) nativeCancel {
	found := false
	if uiThread.Run(func() {
		found = !p.ends.pending(id) && withDownloadOnUI(p, id, func(download unsafe.Pointer) { C.ao_wkv_download_cancel(download) })
	}) == uicall.Withdrawn {
		return nativeCancelWithdrawn
	}
	if !found {
		return nativeCancelEnded
	}
	return nativeCanceled
}

// withDownloadOnUI runs fn with p's download id on the main thread, where
// the caller runs it, unless it was released, and answers whether fn ran.
func withDownloadOnUI(p *wkProfile, id downloadID, fn func(download unsafe.Pointer)) bool {
	p.mu.Lock()
	entry := p.downloads[id]
	p.mu.Unlock()
	if entry == nil {
		return false
	}
	fn(entry.ptr)
	return true
}

func wkReleaseDownload(download unsafe.Pointer) {
	releaseDownloadRef.release(uiThread, func() { C.ao_wkv_download_release(download) })
}

// wkSample is one sample of a running download's progress
// (wkDownloadProgress).
type wkSample struct {
	kind            wkSampleKind
	received, total float64
}

// wkSampleKind is how a sample ended.
type wkSampleKind int

const (
	// wkSampleTaken: the download was sampled; received and total hold.
	wkSampleTaken wkSampleKind = iota + 1
	// wkSampleReleased: the download was released first.
	wkSampleReleased
	// wkSampleWithdrawn: the main thread did not run the sample in time,
	// and it never will.
	wkSampleWithdrawn
)

// wkDownloadProgress samples the NSProgress of entry's download on the
// main thread, while entry is p's (heldDownload): the bytes received and
// the size expected, 0 when unknown. WKDownload reports no per-chunk
// callback, so the profile polls this while a download is live; that
// sample is what lets the Manager cancel one that outgrows its byte cap
// before the whole file is on disk.
func wkDownloadProgress(p *wkProfile, entry *wkDownload) wkSample {
	var r, t C.double
	held := false
	if uiThread.Run(func() {
		download := p.heldDownload(entry)
		if download == nil {
			return
		}
		held = true
		r = C.ao_wkv_download_received(download)
		t = C.ao_wkv_download_total(download)
	}) == uicall.Withdrawn {
		return wkSample{kind: wkSampleWithdrawn}
	}
	if !held {
		return wkSample{kind: wkSampleReleased}
	}
	return wkSample{kind: wkSampleTaken, received: float64(r), total: float64(t)}
}
