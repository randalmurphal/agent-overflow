//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"context"
	"fmt"
	"log"
	"sync"
	"unsafe"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/serialqueue"
)

// The WKWebView engine's process half (spec §6); its profile half is
// wkwebview_profile_darwin.go. It is IN-PROCESS by necessity: on macOS the
// backend is the app process, and the views it hosts are subviews of the app's
// own NSWindow.
//
// One locking rule holds across this file, the profile half and the page
// driver beside them, and every deadlock this design can have is a violation
// of it:
//
//	NEVER call into AppKit or WebKit while holding a lock a delegate can take.
//
// A call on `uiThread` waits for the main thread, and that thread may be
// running a delegate — a popup, a navigation-authority answer, a dialog — that
// wants the very lock being held. So a lock here only ever covers map
// bookkeeping, and every call on uiThread happens after it is released.

// newNativeEngine answers a WKWebView engine only when the caller supplied a
// desktop window to host views inside. The same darwin binary also runs
// without one (`--connect`, a headless serve mode, `go test`), and those get
// no engine and no browser tools, which is why this is a capability answer
// and never a GOOS check.
//
// configDir is deliberately unused here, unlike the WebKitGTK engine. macOS
// exposes no documented way to place a WKWebsiteDataStore in a directory of the
// app's choosing: +dataStoreForIdentifier: (macOS 14) persists to a location
// WebKit owns, and everything older has only the non-persistent store. So the
// AO-owned `browser-profiles/` tree (spec §4) has no macOS counterpart, and
// inventing an empty one would be a directory that documents a lie.
func newNativeEngine(_ string, opts ManagerOptions, events engineEvents) browserEngine {
	if opts.NativeWindow == nil {
		return nil
	}
	return &wkEngine{
		window: opts.NativeWindow, events: events,
		popups: newHeldPopups(), slots: make(map[int]bool),
		pane: make(map[uint64]wkPaneState),
	}
}

type wkEngine struct {
	window func() unsafe.Pointer
	events engineEvents

	// startMu serializes host attachment. Separate from mu on purpose: it is
	// held across a main-thread call, so no delegate may ever take it.
	startMu sync.Mutex
	started bool

	mu     sync.Mutex
	popups *heldPopups
	slots  map[int]bool
	pane   map[uint64]wkPaneState

	// downloadEvents carries every download report to the Manager off the
	// main thread, one at a time in the order they were made, so a fast
	// download's finish never reaches the Manager before its start.
	downloadEvents serialqueue.Queue
}

func (e *wkEngine) Start(context.Context) error {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	if e.started {
		return nil
	}
	window := e.window()
	if window == nil {
		return fmt.Errorf("browser: the desktop window is not ready yet")
	}
	if err := wkAttachHost(window); err != nil {
		return err
	}
	e.started = true
	return nil
}

func (e *wkEngine) Running() bool {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	return e.started
}

// Interrupt has nothing to cancel. Starting this engine is one bounded call on
// the main thread — there is no download, no process launch, and no wait a
// concurrent shutdown could be stuck behind.
func (e *wkEngine) Interrupt() {}

func (e *wkEngine) Stop() {
	e.startMu.Lock()
	e.started = false
	e.startMu.Unlock()
	// The window and its park view stay: the SPA lives in it, and re-attaching
	// is idempotent. Only pages AO created are torn down, and profiles have
	// already been disposed by their owner. A held popup closes through the
	// receipt its discard settles, so a discard made meanwhile answers
	// once the close has run (heldPopups.closeAll).
	ctx, cancel := context.WithTimeout(context.Background(), nativeCallTimeout)
	defer cancel()
	if err := e.popups.closeAll(ctx, wkCloseViewSettled, wkOnMainThread()); err != nil {
		log.Printf("browser: close the held popups at stop: %v", err)
	}
}

// ClearSiteData is this engine's half of the Settings action (engineSiteData in
// driver.go, spec §4). WebKit owns the directory +dataStoreForIdentifier:
// persists into, so the Manager's tree delete reaches none of it and the clear
// has to be WebKit's own removal of every store this app created.
//
// The Manager calls it only after closeBrowser, so nothing is live to refuse
// removal. A stopped or never-started engine still clears: nothing below
// touches a view, the host, or e.mu, so there is no state to be missing and no
// lock to deadlock against.
func (e *wkEngine) ClearSiteData(ctx context.Context) error {
	return wkClearSiteData(ctx)
}

// The Manager finds this capability by TYPE ASSERTION, so a signature that
// drifted would silently stop clearing rather than fail to build.
var _ engineSiteData = (*wkEngine)(nil)

// OnUIThread is the Manager's cue (engineUIThread in driver.go) that every
// call on uiThread would run inline from the caller. It is what lets a teardown
// issued from the blocked main thread at quit finish instead of parking on a
// queue that thread is not draining.
func (e *wkEngine) OnUIThread() bool { return wkOnMainThread() }

var _ engineUIThread = (*wkEngine)(nil)

// WaitDownloadEvents is engineDownloadEvents.
func (e *wkEngine) WaitDownloadEvents() { e.downloadEvents.Wait() }

var _ engineDownloadEvents = (*wkEngine)(nil)

// FocusApp is engineAppFocus: the SPA's webview becomes the first
// responder. Called on the main thread from the key gate, where uiThread
// runs it inline.
func (e *wkEngine) FocusApp() error { return wkFocusApp() }

var _ engineAppFocus = (*wkEngine)(nil)

// holdPopup takes ownership of an engine-created view, opened by the page
// with handle opener, until the Manager adopts or discards it. The engine
// never decides which. It answers the popup's number, the page number its
// adoption gives it, which the view keeps, and its handle.
func (e *wkEngine) holdPopup(view unsafe.Pointer, opener ids.PageHandle) (uint64, ids.PageHandle) {
	n := wkPageSeq.Add(1)
	handle := pageHandle(n)
	e.popups.hold(handle, heldPopupView{view: view, opener: opener, n: n})
	return n, handle
}

// takePopup takes the held popup handle, for its attach.
func (e *wkEngine) takePopup(handle ids.PageHandle) heldPopupView {
	return e.popups.take(handle)
}

// heldOpener answers the opener of the held popup handle, "" for none.
func (e *wkEngine) heldOpener(handle ids.PageHandle) ids.PageHandle {
	return e.popups.opener(handle)
}

// DownloadSaving: WebKit writes into AO's directory and the Manager
// publishes each finished file to the user's Downloads folder.
func (e *wkEngine) DownloadSaving() downloadSaving { return downloadsStaged }

// DiscardPage closes a held popup within ctx, and past it answers that the
// close is pending while the engine still runs it (heldPopups.discard).
func (e *wkEngine) DiscardPage(ctx context.Context, handle ids.PageHandle) error {
	return e.popups.discard(ctx, handle, wkCloseViewSettled)
}

// claimSlot reserves one row in the park view. Parked views are stacked by slot
// inside a 1x1 clipping layer, so two pages must never share one.
func (e *wkEngine) claimSlot() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for slot := 0; ; slot++ {
		if !e.slots[slot] {
			e.slots[slot] = true
			return slot
		}
	}
}

func (e *wkEngine) releaseSlot(slot int) {
	e.mu.Lock()
	delete(e.slots, slot)
	e.mu.Unlock()
}
