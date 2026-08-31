//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/chromedp/cdproto/network"
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
// `wkDo` waits for the main thread, and that thread may be running a delegate —
// a popup, a navigation-authority answer, a dialog — that wants the very lock
// being held. So a lock here only ever covers map bookkeeping, and every
// wkDo-backed call happens after it is released.

const (
	// A background page still needs a real viewport: layout, media queries, and
	// element geometry are all read from it, and a screenshot of a hidden page
	// is taken at this size. It is the size the pane opens at, so presenting a
	// page does not reflow everything it has already measured.
	wkHiddenWidth  = 1280
	wkHiddenHeight = 800

	// wkDownloadPollInterval is how often a live download's NSProgress is
	// sampled. WKDownload has no per-chunk callback, and the Manager's
	// per-download byte cap is only enforceable against a mid-flight number.
	wkDownloadPollInterval = 250 * time.Millisecond
)

// newNativeEngine answers a WKWebView engine only when the caller supplied a
// desktop window to host views inside AND this macOS carries the one API the
// engine is built on. The same darwin binary also runs with no window at all —
// `--connect`, the harness, `go test` — and those keep managed Chrome, which is
// why this is a capability answer and never a GOOS check.
//
// configDir is deliberately unused here, unlike the WebKitGTK engine. macOS
// exposes no documented way to place a WKWebsiteDataStore in a directory of the
// app's choosing: +dataStoreForIdentifier: (macOS 14) persists to a location
// WebKit owns, and everything older has only the non-persistent store. So the
// AO-owned `browser-profiles/` tree (spec §4) has no macOS counterpart, and
// inventing an empty one would be a directory that documents a lie.
func newNativeEngine(_ string, opts ManagerOptions, events engineEvents) browserEngine {
	if opts.NativeWindow == nil || !wkSupported() {
		return nil
	}
	return &wkEngine{
		window: opts.NativeWindow, events: events,
		popups: make(map[string]unsafe.Pointer), slots: make(map[int]bool),
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
	popupSeq uint64
	popups   map[string]unsafe.Pointer
	slots    map[int]bool
	profiles map[uint64]*wkProfile
	stopped  bool
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
	for handle, view := range e.popups {
		orphans = append(orphans, view)
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

// holdPopup takes ownership of an engine-created view until the Manager adopts
// or discards it. The engine never decides which.
func (e *wkEngine) holdPopup(view unsafe.Pointer) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.popupSeq++
	handle := fmt.Sprintf("popup-%d", e.popupSeq)
	e.popups[handle] = view
	return handle
}

func (e *wkEngine) takePopup(handle string) unsafe.Pointer {
	e.mu.Lock()
	defer e.mu.Unlock()
	view := e.popups[handle]
	delete(e.popups, handle)
	return view
}

func (e *wkEngine) DiscardPage(handle string) {
	if view := e.takePopup(handle); view != nil {
		wkCloseView(view)
	}
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
	store, err := wkNewStore(wkStoreIdentifier(opts.Workspace), !opts.Persist)
	if err != nil {
		return nil, err
	}
	profile := &wkProfile{
		engine: e, id: id, handle: fmt.Sprintf("profile-%d", id), store: store,
		downloadDir: opts.DownloadDir,
		pages:       make(map[*wkPage]struct{}), downloads: make(map[string]*wkDownload),
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
	store       unsafe.Pointer
	downloadDir string

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
// in every frame: the checkpoint's localStorage seed, then the console capture
// that stands in for CDP's Runtime and Log domains. The capture script posts to
// `window.webkit.messageHandlers.<handler>`, which WKUserContentController
// spells exactly as WebKitGTK's does — so the builder is shared, not forked.
func wkUserScript(restore map[string]map[string]string) (string, error) {
	seed, err := storageRestoreScript(restore)
	if err != nil {
		return "", err
	}
	return seed + webkitConsoleCaptureScript(webkitConsoleHandler), nil
}

func (p *wkProfile) NewPage(_ context.Context, hooks pageHooks, restore map[string]map[string]string) (pageDriver, error) {
	script, err := wkUserScript(restore)
	if err != nil {
		return nil, err
	}
	page, err := p.newPageShell(hooks)
	if err != nil {
		return nil, err
	}
	view, err := wkNewView(p.store, page.id, p.id, script, webkitConsoleHandler, p.downloadDir)
	if err != nil {
		page.discard()
		return nil, err
	}
	page.view = view
	p.parkNew(page)
	return page, nil
}

func (p *wkProfile) AttachPage(_ context.Context, handle string, hooks pageHooks, restore map[string]map[string]string) (pageDriver, error) {
	view := p.engine.takePopup(handle)
	if view == nil {
		return nil, fmt.Errorf("browser: page %s is no longer available", handle)
	}
	script, err := wkUserScript(restore)
	if err != nil {
		wkCloseView(view)
		return nil, err
	}
	page, err := p.newPageShell(hooks)
	if err != nil {
		wkCloseView(view)
		return nil, err
	}
	page.view = view
	// The popup already exists and may already be loading, so its document start
	// is gone: the injected script applies from its next document on. That is
	// the same trade CDP's addScriptToEvaluateOnNewDocument makes for an adopted
	// target.
	if err := wkAdoptView(view, page.id, p.id, script, webkitConsoleHandler, p.downloadDir); err != nil {
		page.discard()
		wkCloseView(view)
		return nil, err
	}
	p.parkNew(page)
	return page, nil
}

// newPageShell allocates the Go half of a page and registers it BEFORE the view
// exists, so a delegate that fires during construction resolves to a real page
// instead of being dropped.
func (p *wkProfile) newPageShell(hooks pageHooks) (*wkPage, error) {
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return nil, fmt.Errorf("browser: workspace profile is closed")
	}
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	page := &wkPage{
		engine: p.engine, profile: p, id: wkPageSeq.Add(1),
		hooks: hooks, ctx: ctx, cancel: cancel, slot: -1,
	}
	page.handle = fmt.Sprintf("page-%d", page.id)
	wkPageByID.Store(page.id, page)
	p.mu.Lock()
	p.pages[page] = struct{}{}
	p.mu.Unlock()
	return page, nil
}

// parkNew gives a page its slot in the park view. Parked is IN THE WINDOW: the
// view has a real viewport and a snapshot WebKit will actually render, it simply
// costs the window no space. An unparented WKWebView is the trap this avoids —
// WebKit only guarantees layout and snapshots for a view inside a window.
func (p *wkProfile) parkNew(page *wkPage) {
	page.slot = p.engine.claimSlot()
	wkParkView(page.view, page.slot, wkHiddenWidth, wkHiddenHeight)
}

// Cookies has nothing to hand the checkpoint. This engine's site data is its own
// WKWebsiteDataStore, so re-persisting a copy through the encrypted checkpoint
// would be a second, staler source of truth. Spec §4 deletes the checkpoint with
// the CDP engine.
func (p *wkProfile) Cookies(context.Context) ([]*network.CookieParam, error) {
	return nil, nil
}

func (p *wkProfile) registerDownload(handle string, ptr unsafe.Pointer) {
	entry := &wkDownload{handle: handle, ptr: ptr, stop: make(chan struct{})}
	p.mu.Lock()
	closed := p.disposed
	if !closed {
		p.downloads[handle] = entry
	}
	p.mu.Unlock()
	if closed {
		// registerDownload runs INSIDE the delegate, on the main thread, and
		// wkDo would queue behind the very callback it is running in. Every
		// main-thread call this export makes goes to a goroutine for that
		// reason.
		go func() {
			wkCancelDownload(ptr)
			wkReleaseDownload(ptr)
		}()
		return
	}
	wkDownloadOwners.Store(handle, p)
	go p.sampleDownload(entry)
}

// sampleDownload is the WKDownload half of what WebKitGTK reports for free on
// its `received-data` signal. Without it the Manager could only enforce its
// per-download byte cap once the whole file was already written.
func (p *wkProfile) sampleDownload(entry *wkDownload) {
	ticker := time.NewTicker(wkDownloadPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-entry.stop:
			return
		case <-ticker.C:
			received := wkDownloadReceived(entry.ptr)
			select {
			case <-entry.stop:
				return
			default:
			}
			p.engine.events.DownloadProgress(downloadProgress{
				ID: entry.handle, Received: received, State: downloadInProgress,
			})
		}
	}
}

func (p *wkProfile) releaseDownload(handle string) {
	p.mu.Lock()
	entry := p.downloads[handle]
	delete(p.downloads, handle)
	p.mu.Unlock()
	wkDownloadOwners.Delete(handle)
	if entry == nil {
		return
	}
	// Deleting under the lock is what makes exactly one caller reach the close.
	close(entry.stop)
	wkReleaseDownload(entry.ptr)
}

func (p *wkProfile) CancelDownload(id string) {
	p.mu.Lock()
	entry := p.downloads[id]
	p.mu.Unlock()
	if entry != nil {
		wkCancelDownload(entry.ptr)
	}
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
