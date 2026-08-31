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
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// The only cgo in the WebKitGTK engine. Everything above it — the engine, the
// profile, the page driver — is ordinary Go against the small Go-typed surface
// declared here, so exactly one file has to be read with C in mind.
//
// Two rules this file exists to enforce:
//
//  1. EVERY GTK or WebKit call happens on the GTK main thread, through
//     `gtkDo`. Wails owns that thread; calling into GTK from a Go goroutine is
//     undefined behaviour, not a race that shows up under load.
//  2. No Go pointer is ever handed to C. Callbacks address a page or a profile
//     by a uint64 id and this file resolves it, which is also what makes a
//     callback arriving after teardown a lookup miss rather than a crash.

// gtkCallTimeout bounds a main-thread dispatch. The app loop can already be
// gone during shutdown, and a browser operation must fail rather than park a
// goroutine on a queue nothing will drain again.
const gtkCallTimeout = 10 * time.Second

// gtkDo runs fn on the GTK main thread and reports whether it completed. A
// false answer means the loop did not run it in time; fn may still run later,
// so fn must only write to variables the caller reads after a true answer.
// gtkAlive reports whether there is an app loop to dispatch to at all. It is
// the difference between "this will never run" and "this did not run yet",
// which is what decides who owns a C allocation a closure would have freed.
func gtkAlive() bool { return application.Get() != nil }

func gtkDo(fn func()) bool {
	if !gtkAlive() {
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
	case <-time.After(gtkCallTimeout):
		return false
	}
}

var errGTKUnavailable = fmt.Errorf("browser: the desktop window is not accepting browser work")

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
	page := webkitLookupPage(uint64(pageID))
	// Answered OFF the GTK thread: navigation authority is the Manager's, and
	// asking it takes Manager locks — blocking the GTK thread on a Go lock is
	// how the whole window freezes behind one browser operation. The delegate
	// deferred the decision with a reference held for exactly this.
	go func() {
		allow := C.int(0)
		if page != nil && (page.hooks.Allow == nil || page.hooks.Allow(target)) {
			allow = 1
		}
		// A loop that is gone takes the page with it, so an unanswered
		// decision dies with the process rather than blocking anything.
		gtkDo(func() { C.ao_wk_policy_finish(decision, allow) })
	}()
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
	page.noteLoad(location)
	// Off the GTK thread: the Manager takes its own locks to route this.
	go page.engine.events.PageInfoChanged(page.handle, location, name)
}

//export aoWebKitPageClosed
func aoWebKitPageClosed(pageID C.uint64_t) {
	if page := webkitLookupPage(uint64(pageID)); page != nil {
		go page.engine.events.PageClosed(page.handle)
	}
}

//export aoWebKitPopup
func aoWebKitPopup(openerID C.uint64_t, profileID C.uint64_t, view unsafe.Pointer, uri *C.char) {
	target := webkitTakeString(uri)
	opener := webkitLookupPage(uint64(openerID))
	profile := webkitLookupProfile(uint64(profileID))
	if opener == nil || profile == nil {
		// Nothing can own it, so nothing may keep it alive — but not YET, and
		// not from here. This export runs on the GTK thread inside `create`,
		// which is about to return this very widget to WebKit: unreffing it now
		// would hand WebKit a finalized object. And a bare gtkDo would queue the
		// close behind the delegate it is running inside, freezing the UI for a
		// full gtkCallTimeout. The goroutine's dispatch lands on a later GTK
		// turn, which is both safe and immediate.
		go webkitCloseView(view)
		return
	}
	handle := profile.engine.holdPopup(view)
	go profile.engine.events.PopupOpened(enginePopup{
		Profile: profile.Handle(), Opener: opener.handle, Handle: handle, URL: target,
	})
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

//export aoWebKitDownloadStarted
func aoWebKitDownloadStarted(profileID, pageID, downloadID C.uint64_t, download unsafe.Pointer, uri, suggested *C.char) {
	target, name := webkitTakeString(uri), webkitTakeString(suggested)
	profile := webkitLookupProfile(uint64(profileID))
	if profile == nil {
		C.ao_wk_download_cancel(download)
		C.ao_wk_download_unref(download)
		return
	}
	page := webkitLookupPage(uint64(pageID))
	frame := ""
	if page != nil {
		frame = page.handle
	}
	// The handle IS the on-disk name the C side gave the file, which is what
	// lets the Manager find and rename a finished download.
	handle := fmt.Sprintf("dl-%d", uint64(downloadID))
	profile.registerDownload(handle, download)
	if name == "" {
		name = handle
	}
	go profile.engine.events.DownloadStarted(downloadStart{
		Frame: frame, ID: handle, URL: target, SuggestedName: name,
	})
}

//export aoWebKitDownloadProgress
func aoWebKitDownloadProgress(downloadID C.uint64_t, received C.double, state C.int, path *C.char) {
	filePath := webkitTakeString(path)
	handle := fmt.Sprintf("dl-%d", uint64(downloadID))
	// The profile that started the download owns the whole of its progress, so
	// a report arriving after that profile is disposed is a lookup miss rather
	// than an event routed at a torn-down workspace.
	profile := webkitDownloadOwner(handle)
	if profile == nil {
		return
	}
	progress := downloadProgress{ID: handle, Received: float64(received), State: downloadInProgress, FilePath: filePath}
	switch int(state) {
	case 1:
		progress.State = downloadCompleted
	case 2:
		progress.State = downloadCanceled
	}
	if progress.State != downloadInProgress {
		// Released off the GTK thread, in order, before the event goes out:
		// releasing dispatches BACK to the GTK thread, which would queue behind
		// the signal handler this export is running inside and freeze the UI
		// for a full gtkCallTimeout on every finished download.
		go func() {
			profile.releaseDownload(handle)
			profile.engine.events.DownloadProgress(progress)
		}()
		return
	}
	go profile.engine.events.DownloadProgress(progress)
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
	if !gtkDo(func() { ok = C.ao_wk_host_attach(window) == 1 }) {
		return errGTKUnavailable
	}
	if !ok {
		return fmt.Errorf("browser: the desktop window has no webview to host the browser pane beside")
	}
	return nil
}

func webkitParkView(view unsafe.Pointer, slot, width, height int) {
	gtkDo(func() { C.ao_wk_host_park(view, C.int(slot), C.int(width), C.int(height)) })
}

func webkitPresentView(view unsafe.Pointer, x, y, width, height int) error {
	if !gtkDo(func() { C.ao_wk_host_present(view, C.int(x), C.int(y), C.int(width), C.int(height)) }) {
		return errGTKUnavailable
	}
	return nil
}

func webkitHideView(view unsafe.Pointer, slot, width, height int) error {
	if !gtkDo(func() {
		C.ao_wk_host_hide(view)
		C.ao_wk_host_park(view, C.int(slot), C.int(width), C.int(height))
	}) {
		return errGTKUnavailable
	}
	return nil
}

// Every C string below is allocated INSIDE its dispatch closure and freed
// there. A dispatch that misses its deadline still runs afterwards, so a
// string allocated out here and freed on return would be read after free.
func webkitNewSession(dataDir, cacheDir, cookieFile, downloadDir string, ephemeral bool, profileID uint64) (unsafe.Pointer, error) {
	var session unsafe.Pointer
	flag := C.int(0)
	if ephemeral {
		flag = 1
	}
	if !gtkDo(func() {
		cData, cCache := C.CString(dataDir), C.CString(cacheDir)
		cCookie, cDownload := C.CString(cookieFile), C.CString(downloadDir)
		session = C.ao_wk_session_new(cData, cCache, cCookie, cDownload, flag, C.uint64_t(profileID))
		C.free(unsafe.Pointer(cData))
		C.free(unsafe.Pointer(cCache))
		C.free(unsafe.Pointer(cCookie))
		C.free(unsafe.Pointer(cDownload))
	}) {
		return nil, errGTKUnavailable
	}
	if session == nil {
		return nil, fmt.Errorf("browser: create workspace site-data session")
	}
	return session, nil
}

func webkitFreeSession(session unsafe.Pointer) {
	gtkDo(func() { C.ao_wk_session_free(session) })
}

func webkitNewView(session unsafe.Pointer, pageID uint64, userScript, consoleHandler string) (unsafe.Pointer, error) {
	var view unsafe.Pointer
	if !gtkDo(func() {
		cScript, cHandler := C.CString(userScript), C.CString(consoleHandler)
		view = C.ao_wk_view_new(session, C.uint64_t(pageID), cScript, cHandler)
		C.free(unsafe.Pointer(cScript))
		C.free(unsafe.Pointer(cHandler))
	}) {
		return nil, errGTKUnavailable
	}
	if view == nil {
		return nil, fmt.Errorf("browser: create page view")
	}
	return view, nil
}

func webkitAdoptView(view unsafe.Pointer, pageID uint64, userScript, consoleHandler string) error {
	if !gtkDo(func() {
		cScript, cHandler := C.CString(userScript), C.CString(consoleHandler)
		C.ao_wk_view_adopt(view, C.uint64_t(pageID), cScript, cHandler)
		C.free(unsafe.Pointer(cScript))
		C.free(unsafe.Pointer(cHandler))
	}) {
		return errGTKUnavailable
	}
	return nil
}

func webkitCloseView(view unsafe.Pointer) {
	gtkDo(func() { C.ao_wk_view_close(view) })
}

func webkitSetViewSize(view unsafe.Pointer, width, height int) error {
	if !gtkDo(func() { C.ao_wk_view_set_size(view, C.int(width), C.int(height)) }) {
		return errGTKUnavailable
	}
	return nil
}

func webkitLoadURI(view unsafe.Pointer, uri string) error {
	if !gtkDo(func() {
		cURI := C.CString(uri)
		C.ao_wk_view_load_uri(view, cURI)
		C.free(unsafe.Pointer(cURI))
	}) {
		return errGTKUnavailable
	}
	return nil
}

// The history action codes the C side takes.
const (
	webkitHistoryBack    = 0
	webkitHistoryForward = 1
	webkitHistoryReload  = 2
	webkitHistoryStop    = 3
)

func webkitHistory(view unsafe.Pointer, action int) error {
	if !gtkDo(func() { C.ao_wk_view_history(view, C.int(action)) }) {
		return errGTKUnavailable
	}
	return nil
}

func webkitCanGo(view unsafe.Pointer, forward bool) bool {
	flag := C.int(0)
	if forward {
		flag = 1
	}
	answer := false
	gtkDo(func() { answer = C.ao_wk_view_can_go(view, flag) == 1 })
	return answer
}

func webkitIsLoading(view unsafe.Pointer) bool {
	loading := false
	gtkDo(func() { loading = C.ao_wk_view_is_loading(view) == 1 })
	return loading
}

// webkitEvaluate runs one async-function body in the page and returns its JSON
// result. An undefined result is an empty answer, matching what CDP reports
// for a void expression.
func webkitEvaluate(ctx context.Context, view unsafe.Pointer, body string) (string, error) {
	if !gtkAlive() {
		return "", errGTKUnavailable
	}
	id, ch := webkitRegisterCall()
	if !gtkDo(func() {
		cBody := C.CString(body)
		C.ao_wk_view_eval(view, cBody, C.uint64_t(id))
		C.free(unsafe.Pointer(cBody))
	}) {
		webkitCallByID.Delete(id)
		return "", errGTKUnavailable
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

// webkitSnapshot captures the view. It works on a parked view and returns
// fresh pixels after a DOM mutation, which is what makes hidden agent pages
// screenshot-able at all.
func webkitSnapshot(ctx context.Context, view unsafe.Pointer, fullDocument bool) ([]byte, int, int, error) {
	id, ch := webkitRegisterCall()
	flag := C.int(0)
	if fullDocument {
		flag = 1
	}
	if !gtkDo(func() { C.ao_wk_view_snapshot(view, flag, C.uint64_t(id)) }) {
		webkitCallByID.Delete(id)
		return nil, 0, 0, errGTKUnavailable
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

func webkitCancelDownload(download unsafe.Pointer) {
	gtkDo(func() { C.ao_wk_download_cancel(download) })
}

func webkitReleaseDownload(download unsafe.Pointer) {
	gtkDo(func() { C.ao_wk_download_unref(download) })
}
