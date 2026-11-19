//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"agent-overflow/internal/browser/destination"
	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/serialqueue"
)

// The WKWebView engine's process and profile halves (spec §6). It is IN-PROCESS
// by necessity: on macOS the backend is the app process, and the views it hosts
// are subviews of the app's own NSWindow.
//
// One locking rule holds across this file and the page driver beside it, and
// every deadlock this design can have is a violation of it:
//
//	NEVER call into AppKit or WebKit while holding a lock a delegate can take.
//
// A call on `uiThread` waits for the main thread, and that thread may be
// running a delegate — a popup, a navigation-authority answer, a dialog — that
// wants the very lock being held. So a lock here only ever covers map
// bookkeeping, and every call on uiThread happens after it is released.

const (
	// wkDownloadPollInterval is how often a live download's NSProgress is
	// sampled. WKDownload has no per-chunk callback, and the Manager's
	// per-download byte cap is only enforceable against a mid-flight number.
	wkDownloadPollInterval = 250 * time.Millisecond
)

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
		popups: make(map[ids.PageHandle]heldPopupView), slots: make(map[int]bool),
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

	mu       sync.Mutex
	popups   map[ids.PageHandle]heldPopupView
	slots    map[int]bool
	pane     map[uint64]wkPaneState
	profiles map[uint64]*wkProfile
	stopped  bool

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
	e.mu.Lock()
	e.stopped = false
	e.mu.Unlock()
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
	e.mu.Lock()
	orphans := make([]unsafe.Pointer, 0, len(e.popups))
	for handle, held := range e.popups {
		orphans = append(orphans, held.view)
		delete(e.popups, handle)
	}
	e.stopped = true
	e.mu.Unlock()
	// The window and its park view stay: the SPA lives in it, and re-attaching
	// is idempotent. Only pages AO created are torn down, and profiles have
	// already been disposed by their owner.
	for _, view := range orphans {
		wkCloseView(view)
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
	e.mu.Lock()
	defer e.mu.Unlock()
	e.popups[handle] = heldPopupView{view: view, opener: opener, n: n}
	return n, handle
}

// takePopup takes the held popup handle, for its attach or its discard.
func (e *wkEngine) takePopup(handle ids.PageHandle) heldPopupView {
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.popups[handle]
	delete(e.popups, handle)
	return held
}

// heldOpener answers the opener of the held popup handle, "" for none.
func (e *wkEngine) heldOpener(handle ids.PageHandle) ids.PageHandle {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.popups[handle].opener
}

// downloadStart names where a download of this profile began: page, its
// view's page, or, for a popup not adopted yet, the popup's number. A
// popup's download names the popup by its handle and its opener, before
// adoption and after, so the Manager lists it while the popup is not
// (popup_adoption.go).
func (p *wkProfile) downloadStart(page *wkPage, popup uint64) downloadStart {
	var start downloadStart
	switch {
	case page != nil:
		start.Frame = frameID(page.handle)
		if page.popup != "" {
			start.Popup, start.Openers = page.popup, []ids.PageHandle{page.opener}
		}
	case popup != 0:
		start.Popup = pageHandle(popup)
		if opener := p.engine.heldOpener(start.Popup); opener != "" {
			start.Openers = []ids.PageHandle{opener}
		}
	}
	if start.Popup != "" {
		start.Profile = p.Handle()
	}
	return start
}

// DownloadSaving: WebKit writes into AO's directory and the Manager
// publishes each finished file to the user's Downloads folder.
func (e *wkEngine) DownloadSaving() downloadSaving { return downloadsStaged }

func (e *wkEngine) DiscardPage(_ context.Context, handle ids.PageHandle) error {
	if held := e.takePopup(handle); held.view != nil {
		return wkCloseViewErr(held.view)
	}
	return nil
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

func (e *wkEngine) NewProfile(_ context.Context, opts profileOptions) (engineProfile, error) {
	if !e.Running() {
		return nil, fmt.Errorf("browser: engine unavailable")
	}
	id := wkProfileSeq.Add(1)
	store, err := wkNewStore(wkStoreIdentifier(opts.Jar), opts.Ephemeral)
	if err != nil {
		return nil, err
	}
	profile := &wkProfile{
		engine: e, id: id, handle: fmt.Sprintf("profile-%d", id), store: store,
		stage: opts.StageDownload, releaseStaged: opts.ReleaseStaged,
		destinations: destination.NewDecisions[string, wkPendingDownload](),
		pages:        make(map[*wkPage]struct{}), downloads: make(map[string]*wkDownload),
	}
	wkProfileByID.Store(id, profile)
	e.mu.Lock()
	if e.profiles == nil {
		e.profiles = make(map[uint64]*wkProfile)
	}
	e.profiles[id] = profile
	e.mu.Unlock()
	return profile, nil
}

// wkProfile is one workspace's WKWebsiteDataStore: its own cookie jar, cache,
// and download destination, never the default store (which is the SPA webview's
// own site data).
type wkProfile struct {
	engine *wkEngine
	id     uint64
	handle string
	// store is this workspace's WKWebsiteDataStore. It survives a restart only
	// on macOS 14+ with the site-data setting on: WebKit offers no
	// per-identifier persistent store below that, and an in-memory one is the
	// only honest answer there.
	store unsafe.Pointer
	// stage names the staged file each download is written to
	// (profileOptions.StageDownload), and releaseStaged gives up one no
	// download start took.
	stage         func(id string) (string, error)
	releaseStaged func(id string)
	// destinations are the downloads whose destination decision is open:
	// held on the main thread as WebKit hands it over, taken there once by
	// its completion or by Dispose.
	destinations *destination.Decisions[string, wkPendingDownload]

	mu        sync.Mutex
	disposed  bool
	pages     map[*wkPage]struct{}
	downloads map[string]*wkDownload
}

// wkDownload is one live download: the retained WKDownload and the sampler that
// reports its progress until it settles.
type wkDownload struct {
	handle string
	ptr    unsafe.Pointer
	stop   chan struct{}
}

func (p *wkProfile) Handle() string { return p.handle }

// wkUserScript is what every page in this engine has injected at document start
// in every frame: the console capture that stands in for CDP's Runtime and Log
// domains. The capture script posts to
// `window.webkit.messageHandlers.<handler>`, which WKUserContentController
// spells exactly as WebKitGTK's does — so the builder is shared, not forked.
func wkUserScript() string {
	return webkitConsoleCaptureScript(webkitConsoleHandler)
}

func (p *wkProfile) NewPage(_ context.Context, hooks pageHooks) (pageDriver, error) {
	script := wkUserScript()
	page, err := p.newPageShell(hooks, 0)
	if err != nil {
		return nil, err
	}
	view, err := wkNewView(page.id, p.id, script, webkitConsoleHandler)
	if err != nil {
		page.discard()
		return nil, err
	}
	page.view = view
	if err := p.parkNew(page); err != nil {
		return nil, err
	}
	if err := p.publish(page); err != nil {
		return nil, err
	}
	return page, nil
}

func (p *wkProfile) AttachPage(_ context.Context, handle ids.PageHandle, hooks pageHooks) (pageDriver, error) {
	held := p.engine.takePopup(handle)
	view := held.view
	if view == nil {
		return nil, fmt.Errorf("browser: page %s is no longer available", handle)
	}
	script := wkUserScript()
	page, err := p.newPageShell(hooks, held.n)
	if err != nil {
		wkCloseView(view)
		return nil, err
	}
	page.view = view
	page.popup, page.opener = handle, held.opener
	// The popup already exists and may already be loading, so its document start
	// is gone: the injected script applies from its next document on. That is
	// the same trade CDP's addScriptToEvaluateOnNewDocument makes for an adopted
	// target.
	if err := wkAdoptView(view, page.id, p.id, script, webkitConsoleHandler); err != nil {
		page.discard()
		wkCloseView(view)
		return nil, err
	}
	// What ended the view before reached the Manager by the popup's handle,
	// which its page keeps: the Manager's listing reads it.
	if err := p.parkNew(page); err != nil {
		return nil, err
	}
	if err := p.publish(page); err != nil {
		return nil, err
	}
	return page, nil
}

// newPageShell allocates the Go half of a page and registers it with the
// callbacks BEFORE the view exists, so a delegate that fires during
// construction resolves to a real page instead of being dropped. The
// profile lists it for Dispose only once its view is in place (publish). A
// popup's page takes the number it was held under (holdPopup); zero numbers
// a new one.
func (p *wkProfile) newPageShell(hooks pageHooks, id uint64) (*wkPage, error) {
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return nil, fmt.Errorf("browser: workspace profile is closed")
	}
	p.mu.Unlock()
	if id == 0 {
		id = wkPageSeq.Add(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	page := &wkPage{
		engine: p.engine, profile: p, id: id,
		hooks: hooks, ctx: ctx, cancel: cancel, slot: -1,
	}
	page.handle = pageHandle(page.id)
	wkPageByID.Store(page.id, page)
	return page, nil
}

// publish lists a page whose view is built, adopted and parked in the
// profile, where Dispose finds it. A profile disposed meanwhile never saw
// the page, so the page is closed here, its view and slot with it.
func (p *wkProfile) publish(page *wkPage) error {
	p.mu.Lock()
	disposed := p.disposed
	if !disposed {
		p.pages[page] = struct{}{}
	}
	p.mu.Unlock()
	if disposed {
		page.Close()
		return fmt.Errorf("browser: workspace profile is closed")
	}
	return nil
}

// parkNew gives a page its slot in the park view. Parked is IN THE WINDOW: the
// view has a real viewport and a snapshot WebKit will actually render, it simply
// costs the window no space. An unparented WKWebView is the trap this avoids —
// WebKit only guarantees layout and snapshots for a view inside a window. A
// view the main thread did not park in time is never parked: its page is
// closed with it, which gives its slot back.
func (p *wkProfile) parkNew(page *wkPage) error {
	page.slot = p.engine.claimSlot()
	if err := wkParkView(page.view, page.slot); err != nil {
		page.Close()
		return err
	}
	return nil
}

// registerDownload keeps download ptr under handle and starts its sampler,
// unless the profile was disposed, and reports whether it did. Both
// registrations are made in one hold of p.mu, so a Dispose that takes the
// entry also finds, and deletes, its owner (releaseDownload).
func (p *wkProfile) registerDownload(handle string, ptr unsafe.Pointer) bool {
	entry := &wkDownload{handle: handle, ptr: ptr, stop: make(chan struct{})}
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return false
	}
	p.downloads[handle] = entry
	wkDownloadOwners.Store(handle, p)
	p.mu.Unlock()
	go p.sampleDownload(entry)
	return true
}

func (p *wkProfile) isDisposed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disposed
}

// heldDownload answers entry's download while entry is registered, nil
// once it is released.
func (p *wkProfile) heldDownload(entry *wkDownload) unsafe.Pointer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.downloads[entry.handle] != entry {
		return nil
	}
	return entry.ptr
}

// sampleDownload is the WKDownload half of what WebKitGTK reports for free on
// its `received-data` signal: the pane's progress for a running download.
// Each sample looks the download up on the main thread, where a released
// download is let go after it leaves downloads (releaseDownload), so a
// tick that wins the race with the release samples nothing.
func (p *wkProfile) sampleDownload(entry *wkDownload) {
	ticker := time.NewTicker(wkDownloadPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-entry.stop:
			return
		case <-ticker.C:
			sample := wkDownloadProgress(p, entry)
			switch sample.kind {
			case wkSampleWithdrawn:
				continue
			case wkSampleReleased:
				return
			case wkSampleTaken:
			}
			received, total := sample.received, sample.total
			select {
			case <-entry.stop:
				return
			default:
			}
			p.engine.downloadEvents.Go(func() {
				p.engine.events.DownloadProgress(downloadProgress{
					ID: entry.handle, Received: received, Total: total, State: downloadInProgress,
				})
			})
		}
	}
}

func (p *wkProfile) releaseDownload(handle string) {
	p.mu.Lock()
	entry := p.downloads[handle]
	delete(p.downloads, handle)
	wkDownloadOwners.CompareAndDelete(handle, p)
	p.mu.Unlock()
	if entry == nil {
		return
	}
	// Deleting under the lock is what makes exactly one caller reach the close.
	close(entry.stop)
	wkReleaseDownload(entry.ptr)
}

// CancelDownload looks the download up on the main thread, where a
// released download is let go after it leaves downloads (releaseDownload),
// so the one it finds is still held through the cancel.
//
// A download released first has finished, failed or been canceled, and
// its report is on its way (aoWKVDownloadFinished releases before it
// reports).
func (p *wkProfile) CancelDownload(id string) error {
	return wkCancelDownload(p, id).err(errWKUnavailable)
}

func (p *wkProfile) Dispose(context.Context) error {
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return nil
	}
	p.disposed = true
	pages := make([]*wkPage, 0, len(p.pages))
	for page := range p.pages {
		pages = append(pages, page)
	}
	p.pages = make(map[*wkPage]struct{})
	handles := make([]string, 0, len(p.downloads))
	for handle := range p.downloads {
		handles = append(handles, handle)
	}
	p.mu.Unlock()
	for _, page := range pages {
		page.Close()
	}
	for _, handle := range handles {
		p.releaseDownload(handle)
	}
	// Before the store goes: a destination still being decided is canceled
	// and released on the main thread, where its worker's completion would
	// have taken it.
	wkCancelDestinations(p)
	wkProfileByID.Delete(p.id)
	p.engine.mu.Lock()
	delete(p.engine.profiles, p.id)
	p.engine.mu.Unlock()
	wkFreeStore(p.store)
	return nil
}

// wkDownloadOwners maps a download handle to the profile that started it,
// because a terminal report arrives from a delegate with no Go receiver of its
// own. A report for a disposed profile is a miss, not a routed event.
var wkDownloadOwners sync.Map

func wkDownloadOwner(handle string) *wkProfile {
	if value, ok := wkDownloadOwners.Load(handle); ok {
		return value.(*wkProfile)
	}
	return nil
}
