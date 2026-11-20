//go:build linux && cgo && !gtk3 && !android && !server && !nogui

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

// The WebKitGTK engine's process half (spec §6); its profile half is
// webkit_profile_linux.go. It is IN-PROCESS by necessity: on Linux the backend
// is the app process, and the views it hosts are children of the app's own
// GtkWindow.
//
// One locking rule holds across this file, the profile half and the page
// driver beside them, and every deadlock this design can have is a violation
// of it:
//
//	NEVER call into GTK while holding a lock a C callback can take.
//
// A call on `uiThread` waits for the GTK main thread, and that thread may be
// running a delegate — a popup, a navigation-authority answer — that wants the
// very lock being held. So a lock here only ever covers map bookkeeping, and
// every call on uiThread happens after it is released.

// newNativeEngine answers a WebKitGTK engine only when the caller supplied a
// desktop window to host views inside. The same Linux binary also runs with no
// window at all (`--connect`, the harness, `go test`), and selectEngine gives
// those the headless engine when serve boot asks for it and no engine
// otherwise, which is why this is a capability answer and never a GOOS check.
func newNativeEngine(configDir string, opts ManagerOptions, events engineEvents) browserEngine {
	if opts.NativeWindow == nil {
		return nil
	}
	return &webkitEngine{
		configDir: configDir, window: opts.NativeWindow, events: events,
		popups: newHeldPopups(),
	}
}

type webkitEngine struct {
	configDir string
	window    func() unsafe.Pointer
	events    engineEvents

	// startMu serializes host attachment. It is held across a GTK call, so
	// no callback may ever take it.
	startMu sync.Mutex
	started bool

	popups *heldPopups
	pane   nativePanes

	// downloadEvents carries every download report to the Manager off the
	// GTK thread, one at a time in the order they were made, so a fast
	// download's finish never reaches the Manager before its start.
	downloadEvents serialqueue.Queue
}

func (e *webkitEngine) Start(context.Context) error {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	if e.started {
		return nil
	}
	window := e.window()
	if window == nil {
		return fmt.Errorf("browser: the desktop window is not ready yet")
	}
	if err := webkitAttachHost(window); err != nil {
		return err
	}
	e.started = true
	return nil
}

func (e *webkitEngine) Running() bool {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	return e.started
}

// Interrupt has nothing to cancel. Starting this engine is one bounded call on
// the GTK thread — there is no download, no process launch, and no wait a
// concurrent shutdown could be stuck behind.
func (e *webkitEngine) Interrupt() {}

func (e *webkitEngine) Stop() {
	e.startMu.Lock()
	e.started = false
	e.startMu.Unlock()
	// The window and its overlay stay: the SPA lives in it, and re-attaching
	// is idempotent. Only pages AO created are torn down, and profiles have
	// already been disposed by their owner. A held popup closes through the
	// receipt its discard settles, so a discard made meanwhile answers
	// once the close has run (heldPopups.closeAll).
	ctx, cancel := context.WithTimeout(context.Background(), nativeCallTimeout)
	defer cancel()
	if err := e.popups.closeAll(ctx, webkitCloseViewSettled, gtkOnMainThread()); err != nil {
		log.Printf("browser: close the held popups at stop: %v", err)
	}
}

// holdPopup takes ownership of an engine-created view, opened by the page
// with handle opener, until the Manager adopts or discards it. The engine
// never decides which. It answers the popup's number, the page number its
// adoption gives it, which the view keeps, and its handle.
func (e *webkitEngine) holdPopup(view unsafe.Pointer, opener ids.PageHandle) (uint64, ids.PageHandle) {
	n := webkitPageSeq.Add(1)
	handle := pageHandle(n)
	e.popups.hold(handle, heldPopupView{view: view, opener: opener, n: n})
	return n, handle
}

// takePopup takes the held popup handle, for its attach.
func (e *webkitEngine) takePopup(handle ids.PageHandle) heldPopupView {
	return e.popups.take(handle)
}

// heldOpener answers the opener of the held popup handle, "" for none.
func (e *webkitEngine) heldOpener(handle ids.PageHandle) ids.PageHandle {
	return e.popups.opener(handle)
}

// DownloadSaving: WebKit writes into AO's directory and the Manager
// publishes each finished file to the user's Downloads folder.
func (e *webkitEngine) DownloadSaving() downloadSaving { return downloadsStaged }

// DiscardPage closes a held popup within ctx, and past it answers that the
// close is pending while the engine still runs it (heldPopups.discard).
func (e *webkitEngine) DiscardPage(ctx context.Context, handle ids.PageHandle) error {
	return e.popups.discard(ctx, handle, webkitCloseViewSettled)
}

// OnUIThread is the Manager's cue (engineUIThread in driver.go) that every
// call on uiThread would run inline from the caller. It is what lets a teardown issued
// from the blocked main thread at quit finish instead of parking on a queue
// that thread is not draining.
func (e *webkitEngine) OnUIThread() bool { return gtkOnMainThread() }

var _ engineUIThread = (*webkitEngine)(nil)

// WaitDownloadEvents is engineDownloadEvents.
func (e *webkitEngine) WaitDownloadEvents() { e.downloadEvents.Wait() }

var _ engineDownloadEvents = (*webkitEngine)(nil)

// FocusApp is engineAppFocus: the SPA's webview becomes the window's focus
// widget. Called on the GTK thread from the key gate, where uiThread runs
// it inline. In a window that is not active GTK only records the focus
// widget, which takes the keys once the window is activated.
func (e *webkitEngine) FocusApp() error { return webkitFocusApp() }

var _ engineAppFocus = (*webkitEngine)(nil)
