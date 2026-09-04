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
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"agent-overflow/internal/keybindings"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// The only cgo in the WKWebView engine. Everything above it — the engine, the
// profile, the page driver — is ordinary Go against the small Go-typed surface
// declared here, so exactly one file has to be read with Objective-C in mind.
//
// Two rules this file exists to enforce, the same two the WebKitGTK engine's
// cgo file enforces:
//
//  1. EVERY WebKit or AppKit call happens on the main thread, through `wkDo`.
//     Cocoa owns that thread; calling UIKit-class APIs from a Go goroutine is
//     undefined behaviour, not a race that shows up under load.
//  2. No Go pointer is ever handed to C. Callbacks address a page, profile,
//     call, or download by a uint64 id and this file resolves it, which is also
//     what makes a callback arriving after teardown a lookup miss rather than a
//     crash.

// wkCallTimeout bounds a main-thread dispatch. The app loop can already be gone
// during shutdown, and a browser operation must fail rather than park a
// goroutine on a queue nothing will drain again.
const wkCallTimeout = 10 * time.Second

// wkClearTimeout bounds the whole site-data clear. wkCallTimeout bounds ONE
// main-thread dispatch; this covers WebKit's own asynchronous removal of every
// store it holds — one round trip per workspace the user has ever opened — so
// it is deliberately longer. The Settings button must fail rather than park its
// caller forever on a WebKit callback that never arrives.
const wkClearTimeout = 30 * time.Second

// wkAlive reports whether there is an app loop to dispatch to at all. It is the
// difference between "this will never run" and "this did not run yet", which is
// what decides who owns a C allocation a closure would have freed.
func wkAlive() bool { return application.Get() != nil }

// wkDo runs fn on the AppKit main thread and reports whether it completed. A
// false answer means the loop did not run it in time; fn may still run later, so
// fn must only write to variables the caller reads after a true answer.
func wkDo(fn func()) bool {
	if !wkAlive() {
		return false
	}
	done := make(chan struct{})
	application.InvokeAsync(func() {
		defer close(done)
		fn()
	})
	select {
	case <-done:
		return true
	case <-time.After(wkCallTimeout):
		return false
	}
}

var errWKUnavailable = fmt.Errorf("browser: the desktop window is not accepting browser work")

// wkSupported answers whether this macOS carries the one API the engine cannot
// exist without. It is a pure runtime version read with no UI in it, so it does
// not need the main thread — and it runs before any Manager exists.
func wkSupported() bool { return C.ao_wkv_supported() == 1 }

// wkOnMainThread answers whether wkDo would run its closure inline right now —
// true only on the AppKit main thread, where Wails' dispatch short-circuits.
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

//export aoWKVClearDone
func aoWKVClearDone(callID C.uint64_t, errText *C.char) {
	// Lands on the main queue, like every other export here, and does nothing
	// but the registry lookup and one buffered send — never a wkDo, which would
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
func aoWKVAllow(pageID C.uint64_t, decision unsafe.Pointer, uri *C.char, download C.int) {
	target := wkTakeString(uri)
	page := wkLookupPage(uint64(pageID))
	// Answered OFF the main thread: navigation authority is the Manager's, and
	// asking it takes Manager locks — blocking the main thread on a Go lock is
	// how the whole window freezes behind one browser operation. The delegate
	// deferred the decision with a copied block held for exactly this.
	go func() {
		verdict := C.int(C.AO_POLICY_CANCEL)
		if page != nil && (page.hooks.Allow == nil || page.hooks.Allow(target)) {
			// The Manager's authority is over the URL; whether an allowed URL
			// navigates or downloads is what the anchor asked for.
			verdict = C.AO_POLICY_ALLOW
			if download != 0 {
				verdict = C.AO_POLICY_DOWNLOAD
			}
		}
		// A loop that is gone takes the page with it, so an unanswered decision
		// dies with the process rather than blocking anything.
		wkDo(func() { C.ao_wkv_policy_finish(decision, verdict) })
	}()
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
	page.noteLoad(location)
	// Off the main thread: the Manager takes its own locks to route this.
	go page.engine.events.PageInfoChanged(page.handle, location, name)
}

// aoWKVKeyChord runs ON the main thread, inside the key event's delivery, and
// answers synchronously: it is a set lookup in the Manager and nothing that
// waits. The routing that follows a claim leaves the thread on a goroutine.
//
//export aoWKVKeyChord
func aoWKVKeyChord(pageID C.uint64_t, key *C.char, ctrl, meta, alt, shift C.int) C.int {
	page := wkLookupPage(uint64(pageID))
	if page == nil || page.engine.events.KeyChord == nil {
		return 0
	}
	pressed := keybindings.Accelerator{Key: C.GoString(key), Ctrl: ctrl != 0, Meta: meta != 0, Alt: alt != 0, Shift: shift != 0}
	if page.engine.events.KeyChord(page.handle, pressed) {
		return 1
	}
	return 0
}

//export aoWKVPageClosed
func aoWKVPageClosed(pageID C.uint64_t) {
	if page := wkLookupPage(uint64(pageID)); page != nil {
		go page.engine.events.PageClosed(page.handle)
	}
}

//export aoWKVPopup
func aoWKVPopup(openerID C.uint64_t, profileID C.uint64_t, view unsafe.Pointer, uri *C.char) {
	target := wkTakeString(uri)
	opener := wkLookupPage(uint64(openerID))
	profile := wkLookupProfile(uint64(profileID))
	if opener == nil || profile == nil {
		// Nothing can own it, so nothing may keep it alive — but not YET. This
		// export runs inside -createWebViewWithConfiguration:, which is about to
		// return this very view to WebKit; releasing it here would hand WebKit a
		// dead object. The goroutine's dispatch lands on a later main-thread
		// turn, after the delegate has returned.
		go wkCloseView(view)
		return
	}
	handle := profile.engine.holdPopup(view)
	go profile.engine.events.PopupOpened(enginePopup{
		Profile: profile.Handle(), Opener: opener.handle, Handle: handle, URL: target,
	})
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

//export aoWKVDownloadStarted
func aoWKVDownloadStarted(profileID, pageID, downloadID C.uint64_t, download unsafe.Pointer, uri, suggested *C.char) {
	target, name := wkTakeString(uri), wkTakeString(suggested)
	profile := wkLookupProfile(uint64(profileID))
	if profile == nil {
		C.ao_wkv_download_cancel(download)
		C.ao_wkv_download_release(download)
		return
	}
	page := wkLookupPage(uint64(pageID))
	frame := ""
	if page != nil {
		frame = page.handle
	}
	// The handle IS the on-disk name the Objective-C side gave the file, which
	// is what lets the Manager find and rename a finished download.
	handle := fmt.Sprintf("dl-%d", uint64(downloadID))
	profile.registerDownload(handle, download)
	if name == "" {
		name = handle
	}
	go profile.engine.events.DownloadStarted(downloadStart{
		Frame: frame, ID: handle, URL: target, SuggestedName: name,
	})
}

//export aoWKVDownloadFinished
func aoWKVDownloadFinished(downloadID C.uint64_t, received C.double, state C.int, path *C.char) {
	filePath := wkTakeString(path)
	handle := fmt.Sprintf("dl-%d", uint64(downloadID))
	// The profile that started the download owns the whole of its progress, so a
	// report arriving after that profile is disposed is a lookup miss rather
	// than an event routed at a torn-down workspace.
	profile := wkDownloadOwner(handle)
	if profile == nil {
		return
	}
	progress := downloadProgress{ID: handle, Received: float64(received), State: downloadCompleted, FilePath: filePath}
	if int(state) == 2 {
		progress.State = downloadCanceled
	}
	// Both halves go off the main thread: releasing the download dispatches
	// BACK to the main thread, which would queue behind the delegate this
	// export is running inside, and routing the event takes Manager locks.
	go func() {
		profile.releaseDownload(handle)
		profile.engine.events.DownloadProgress(progress)
	}()
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

// wkAttachHost adds AO's clipping park view beneath the Wails window's own
// subviews. The SPA webview is untouched: unlike GTK4, AppKit lets a second
// view join an existing content view without any reparenting surgery.
func wkAttachHost(window unsafe.Pointer) error {
	ok := false
	if !wkDo(func() { ok = C.ao_wkv_host_attach(window) == 1 }) {
		return errWKUnavailable
	}
	if !ok {
		return fmt.Errorf("browser: the desktop window has no content view to host the browser pane in")
	}
	return nil
}

func wkParkView(view unsafe.Pointer, slot, width, height int) {
	wkDo(func() { C.ao_wkv_host_park(view, C.int(slot), C.int(width), C.int(height)) })
}

// wkPresentView moves the view over the pane's rect, cropped to the rect's
// visible intersection and carrying the pane's background colour. The colour is
// parsed HERE rather than on the main thread: the Objective-C half takes the
// packed value and never a string.
func wkPresentView(view unsafe.Pointer, rect PaneRect) error {
	background := C.int(wkBackgroundCode(rect.Background))
	if !wkDo(func() {
		C.ao_wkv_host_present(view, C.double(rect.X), C.double(rect.Y),
			C.double(rect.Width), C.double(rect.Height),
			C.double(rect.ClipX), C.double(rect.ClipY),
			C.double(rect.ClipWidth), C.double(rect.ClipHeight),
			C.double(rect.ViewportWidth), C.double(rect.ViewportHeight),
			background)
	}) {
		return errWKUnavailable
	}
	return nil
}

func wkHideView(view unsafe.Pointer, slot, width, height int) error {
	if !wkDo(func() {
		C.ao_wkv_host_hide(view)
		C.ao_wkv_host_park(view, C.int(slot), C.int(width), C.int(height))
	}) {
		return errWKUnavailable
	}
	return nil
}

// Every C string below is allocated INSIDE its dispatch closure and freed
// there. A dispatch that misses its deadline still runs afterwards, so a string
// allocated out here and freed on return would be read after free.
func wkNewStore(identifier string, ephemeral bool) (unsafe.Pointer, error) {
	var store unsafe.Pointer
	flag := C.int(0)
	if ephemeral {
		flag = 1
	}
	if !wkDo(func() {
		cID := C.CString(identifier)
		store = C.ao_wkv_store_new(cID, flag)
		C.free(unsafe.Pointer(cID))
	}) {
		return nil, errWKUnavailable
	}
	if store == nil {
		return nil, fmt.Errorf("browser: create workspace site-data store")
	}
	return store, nil
}

func wkFreeStore(store unsafe.Pointer) {
	wkDo(func() { C.ao_wkv_store_free(store) })
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
// need is the app loop wkDo dispatches to — always there in the desktop app,
// and a build without one has no engine to reach this through.
func wkClearSiteData(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, wkClearTimeout)
	defer cancel()
	if !wkAlive() {
		return errWKUnavailable
	}
	id, ch := wkRegisterCall()
	if !wkDo(func() { C.ao_wkv_clear_data(C.uint64_t(id)) }) {
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

func wkNewView(store unsafe.Pointer, pageID, profileID uint64, userScript, consoleHandler, downloadDir string) (unsafe.Pointer, error) {
	var view unsafe.Pointer
	if !wkDo(func() {
		cScript, cHandler := C.CString(userScript), C.CString(consoleHandler)
		cDownload := C.CString(downloadDir)
		view = C.ao_wkv_view_new(store, C.uint64_t(pageID), C.uint64_t(profileID), cScript, cHandler, cDownload)
		C.free(unsafe.Pointer(cScript))
		C.free(unsafe.Pointer(cHandler))
		C.free(unsafe.Pointer(cDownload))
	}) {
		return nil, errWKUnavailable
	}
	if view == nil {
		return nil, fmt.Errorf("browser: create page view")
	}
	return view, nil
}

func wkAdoptView(view unsafe.Pointer, pageID, profileID uint64, userScript, consoleHandler, downloadDir string) error {
	if !wkDo(func() {
		cScript, cHandler := C.CString(userScript), C.CString(consoleHandler)
		cDownload := C.CString(downloadDir)
		C.ao_wkv_view_adopt(view, C.uint64_t(pageID), C.uint64_t(profileID), cScript, cHandler, cDownload)
		C.free(unsafe.Pointer(cScript))
		C.free(unsafe.Pointer(cHandler))
		C.free(unsafe.Pointer(cDownload))
	}) {
		return errWKUnavailable
	}
	return nil
}

func wkCloseView(view unsafe.Pointer) {
	wkDo(func() { C.ao_wkv_view_close(view) })
}

func wkSetViewSize(view unsafe.Pointer, width, height int) error {
	if !wkDo(func() { C.ao_wkv_view_set_size(view, C.int(width), C.int(height)) }) {
		return errWKUnavailable
	}
	return nil
}

func wkViewSize(view unsafe.Pointer) (int, int, error) {
	var width, height C.int
	if !wkDo(func() { C.ao_wkv_view_get_size(view, &width, &height) }) {
		return 0, 0, errWKUnavailable
	}
	return int(width), int(height), nil
}

func wkLoadURI(view unsafe.Pointer, uri string) error {
	if !wkDo(func() {
		cURI := C.CString(uri)
		C.ao_wkv_view_load_uri(view, cURI)
		C.free(unsafe.Pointer(cURI))
	}) {
		return errWKUnavailable
	}
	return nil
}

// wkLoadFile is the file:// path. WKWebView refuses a file URL handed to
// -loadRequest:; -loadFileURL:allowingReadAccessToURL: is the documented way,
// and the read-access root is what lets the page reach its own siblings.
func wkLoadFile(view unsafe.Pointer, path, readAccessDir string) error {
	if !wkDo(func() {
		cPath, cRoot := C.CString(path), C.CString(readAccessDir)
		C.ao_wkv_view_load_file(view, cPath, cRoot)
		C.free(unsafe.Pointer(cPath))
		C.free(unsafe.Pointer(cRoot))
	}) {
		return errWKUnavailable
	}
	return nil
}

// The history action codes the Objective-C side takes.
const (
	wkHistoryBack    = 0
	wkHistoryForward = 1
	wkHistoryReload  = 2
	wkHistoryStop    = 3
)

func wkHistory(view unsafe.Pointer, action int) error {
	if !wkDo(func() { C.ao_wkv_view_history(view, C.int(action)) }) {
		return errWKUnavailable
	}
	return nil
}

func wkCanGo(view unsafe.Pointer, forward bool) bool {
	flag := C.int(0)
	if forward {
		flag = 1
	}
	answer := false
	wkDo(func() { answer = C.ao_wkv_view_can_go(view, flag) == 1 })
	return answer
}

func wkIsLoading(view unsafe.Pointer) bool {
	loading := false
	wkDo(func() { loading = C.ao_wkv_view_is_loading(view) == 1 })
	return loading
}

// wkEvaluate runs one async-function body in the page and returns its JSON
// result. An undefined result is an empty answer, matching what CDP reports for
// a void expression.
func wkEvaluate(ctx context.Context, view unsafe.Pointer, body string) (string, error) {
	if !wkAlive() {
		return "", errWKUnavailable
	}
	id, ch := wkRegisterCall()
	if !wkDo(func() {
		cBody := C.CString(body)
		C.ao_wkv_view_eval(view, cBody, C.uint64_t(id))
		C.free(unsafe.Pointer(cBody))
	}) {
		wkCallByID.Delete(id)
		return "", errWKUnavailable
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
func wkSnapshot(ctx context.Context, view unsafe.Pointer) ([]byte, int, int, error) {
	id, ch := wkRegisterCall()
	if !wkDo(func() { C.ao_wkv_view_snapshot(view, C.uint64_t(id)) }) {
		wkCallByID.Delete(id)
		return nil, 0, 0, errWKUnavailable
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

func wkCancelDownload(download unsafe.Pointer) {
	wkDo(func() { C.ao_wkv_download_cancel(download) })
}

func wkReleaseDownload(download unsafe.Pointer) {
	wkDo(func() { C.ao_wkv_download_release(download) })
}

// wkDownloadReceived samples one download's NSProgress. WKDownload reports no
// per-chunk callback, so the profile polls this while a download is live; that
// sample is what lets the Manager cancel one that outgrows its byte cap before
// the whole file is on disk.
func wkDownloadReceived(download unsafe.Pointer) float64 {
	var received C.double
	wkDo(func() { received = C.ao_wkv_download_received(download) })
	return float64(received)
}
