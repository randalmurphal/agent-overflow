//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"agent-overflow/internal/browser/destination"
	"agent-overflow/internal/serialqueue"
)

// The WebKitGTK engine's process and profile halves (spec §6). It is IN-PROCESS
// by necessity: on Linux the backend is the app process, and the views it hosts
// are children of the app's own GtkWindow.
//
// One locking rule holds across this file and the page driver beside it, and
// every deadlock this design can have is a violation of it:
//
//	NEVER call into GTK while holding a lock a C callback can take.
//
// A call on `uiThread` waits for the GTK main thread, and that thread may be
// running a delegate — a popup, a navigation-authority answer — that wants the
// very lock being held. So a lock here only ever covers map bookkeeping, and
// every call on uiThread happens after it is released.

// newNativeEngine answers a WebKitGTK engine only when the caller supplied a
// desktop window to host views inside. The same Linux binary also runs with no
// window at all — `--connect`, the harness, `go test` — and those keep managed
// Chrome, which is why this is a capability answer and never a GOOS check.
func newNativeEngine(configDir string, opts ManagerOptions, events engineEvents) browserEngine {
	if opts.NativeWindow == nil {
		return nil
	}
	return &webkitEngine{
		configDir: configDir, window: opts.NativeWindow, events: events,
		popups: make(map[string]heldPopupView),
		pane:   make(map[uint64]webkitPaneState),
	}
}

type webkitEngine struct {
	configDir string
	window    func() unsafe.Pointer
	events    engineEvents

	// startMu serializes host attachment. Separate from mu on purpose: it is
	// held across a GTK call, so no callback may ever take it.
	startMu sync.Mutex
	started bool

	mu       sync.Mutex
	popups   map[string]heldPopupView
	profiles map[uint64]*webkitProfile
	pane     map[uint64]webkitPaneState
	stopped  bool

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
	e.mu.Lock()
	e.stopped = false
	e.mu.Unlock()
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
	e.mu.Lock()
	orphans := make([]unsafe.Pointer, 0, len(e.popups))
	for handle, held := range e.popups {
		orphans = append(orphans, held.view)
		delete(e.popups, handle)
	}
	e.stopped = true
	e.mu.Unlock()
	// The window and its overlay stay: the SPA lives in it, and re-attaching
	// is idempotent. Only pages AO created are torn down, and profiles have
	// already been disposed by their owner.
	for _, view := range orphans {
		webkitCloseView(view)
	}
}

// holdPopup takes ownership of an engine-created view, opened by the page
// with handle opener, until the Manager adopts or discards it. The engine
// never decides which. It answers the popup's number, the page number its
// adoption gives it, which the view keeps, and its handle.
func (e *webkitEngine) holdPopup(view unsafe.Pointer, opener string) (uint64, string) {
	n := webkitPageSeq.Add(1)
	handle := pageHandle(n)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.popups[handle] = heldPopupView{view: view, opener: opener, n: n}
	return n, handle
}

// takePopup takes the held popup handle, for its attach or its discard.
func (e *webkitEngine) takePopup(handle string) heldPopupView {
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.popups[handle]
	delete(e.popups, handle)
	return held
}

// heldOpener answers the opener of the held popup handle, "" for none.
func (e *webkitEngine) heldOpener(handle string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.popups[handle].opener
}

// downloadStart names where a download of this profile began: page, its
// view's page, or, for a popup not adopted yet, the popup's number. A
// popup's download names the popup by its handle and its opener, before
// adoption and after, so the Manager lists it while the popup is not
// (popup_adoption.go).
func (p *webkitProfile) downloadStart(page *webkitPage, popup uint64) downloadStart {
	var start downloadStart
	switch {
	case page != nil:
		start.Frame = page.handle
		if page.popup != "" {
			start.Popup, start.Openers = page.popup, []string{page.opener}
		}
	case popup != 0:
		start.Popup = pageHandle(popup)
		if opener := p.engine.heldOpener(start.Popup); opener != "" {
			start.Openers = []string{opener}
		}
	}
	if start.Popup != "" {
		start.Profile = p.Handle()
	}
	return start
}

// DownloadSaving: WebKit writes into AO's directory and the Manager
// publishes each finished file to the user's Downloads folder.
func (e *webkitEngine) DownloadSaving() downloadSaving { return downloadsStaged }

func (e *webkitEngine) DiscardPage(_ context.Context, handle string) error {
	if held := e.takePopup(handle); held.view != nil {
		return webkitCloseViewErr(held.view)
	}
	return nil
}

func (e *webkitEngine) NewProfile(_ context.Context, opts profileOptions) (engineProfile, error) {
	if !e.Running() {
		return nil, fmt.Errorf("browser: engine unavailable")
	}
	id := webkitProfileSeq.Add(1)
	// browserProfileDir is the same constant ClearSiteData deletes: the two
	// must name one tree or clearing site data would miss the engine's.
	root := filepath.Join(e.configDir, browserProfileDir, jarDirName(opts.Jar))
	dataDir := filepath.Join(root, "data")
	cacheDir := filepath.Join(root, "cache")
	cookieFile := filepath.Join(dataDir, "cookies.sqlite")
	if !opts.Ephemeral {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("browser: create site data directory: %w", err)
		}
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			return nil, fmt.Errorf("browser: create site cache directory: %w", err)
		}
	}
	// An ephemeral session keeps everything in memory and touches no path at
	// all, which is what the site-data setting being off has to mean for an
	// engine whose storage is its own.
	session, err := webkitNewSession(dataDir, cacheDir, cookieFile, opts.Ephemeral, id)
	if err != nil {
		return nil, err
	}
	profile := &webkitProfile{
		engine: e, id: id, handle: fmt.Sprintf("profile-%d", id), session: session,
		stage: opts.StageDownload, releaseStaged: opts.ReleaseStaged,
		destinations: destination.NewDecisions[string, webkitPendingDownload](),
		pages:        make(map[*webkitPage]struct{}), downloads: make(map[string]unsafe.Pointer),
	}
	webkitProfileByID.Store(id, profile)
	e.mu.Lock()
	if e.profiles == nil {
		e.profiles = make(map[uint64]*webkitProfile)
	}
	e.profiles[id] = profile
	e.mu.Unlock()
	return profile, nil
}

// webkitProfile is one jar's WebKitNetworkSession: its own cookie jar,
// cache, and download destination, never the process-wide default session
// (which would write into the user's ~/.local/share/webkitgtk).
type webkitProfile struct {
	engine  *webkitEngine
	id      uint64
	handle  string
	session unsafe.Pointer

	// stage names the staged file each download is written to
	// (profileOptions.StageDownload), and releaseStaged gives up one no
	// download start took.
	stage         func(id string) (string, error)
	releaseStaged func(id string)
	// destinations are the downloads whose destination decision is open:
	// held on the GTK thread as WebKit hands it over, taken there once by
	// its completion or by Dispose.
	destinations *destination.Decisions[string, webkitPendingDownload]

	mu        sync.Mutex
	disposed  bool
	pages     map[*webkitPage]struct{}
	downloads map[string]unsafe.Pointer

	// beforeView, when set, runs between a new page's shell and its view's
	// construction: a test seam.
	beforeView func()
}

func (p *webkitProfile) Handle() string { return p.handle }

// webkitUserScript is what every page in this engine has injected at document
// start in every frame: the console capture that stands in for CDP's Runtime
// and Log domains.
func webkitUserScript() string {
	return webkitConsoleCaptureScript(webkitConsoleHandler)
}

func (p *webkitProfile) NewPage(_ context.Context, hooks pageHooks) (pageDriver, error) {
	script := webkitUserScript()
	page, err := p.newPageShell(hooks, 0)
	if err != nil {
		return nil, err
	}
	if p.beforeView != nil {
		p.beforeView()
	}
	view, err := webkitNewView(p.id, page.id, script, webkitConsoleHandler)
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

func (p *webkitProfile) AttachPage(_ context.Context, handle string, hooks pageHooks) (pageDriver, error) {
	held := p.engine.takePopup(handle)
	view := held.view
	if view == nil {
		return nil, fmt.Errorf("browser: page %s is no longer available", handle)
	}
	script := webkitUserScript()
	page, err := p.newPageShell(hooks, held.n)
	if err != nil {
		webkitCloseView(view)
		return nil, err
	}
	page.view = view
	page.popup, page.opener = handle, held.opener
	// The popup already exists and may already be loading, so its document
	// start is gone: the injected script applies from its next document on.
	// That is the same trade CDP's addScriptToEvaluateOnNewDocument makes for
	// an adopted target.
	if err := webkitAdoptView(view, page.id, script, webkitConsoleHandler); err != nil {
		page.discard()
		webkitCloseView(view)
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
func (p *webkitProfile) newPageShell(hooks pageHooks, id uint64) (*webkitPage, error) {
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return nil, fmt.Errorf("browser: workspace profile is closed")
	}
	p.mu.Unlock()
	if id == 0 {
		id = webkitPageSeq.Add(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	page := &webkitPage{
		engine: p.engine, profile: p, id: id,
		hooks: hooks, ctx: ctx, cancel: cancel,
	}
	page.handle = pageHandle(page.id)
	webkitPageByID.Store(page.id, page)
	return page, nil
}

// publish lists a page whose view is built, adopted and parked in the
// profile, where Dispose finds it. A profile disposed meanwhile never saw
// the page, so the page is closed here, its view with it.
func (p *webkitProfile) publish(page *webkitPage) error {
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

// parkNew parks a new page's view. Parked is MAPPED: the view has a real
// viewport and a fresh snapshot, it simply costs the window no space. Hiding
// by opacity is banned — it stops rAF, which stops the page. A view the GTK
// thread did not park in time is never parked: its page is closed with it.
func (p *webkitProfile) parkNew(page *webkitPage) error {
	if err := webkitParkView(page.view); err != nil {
		page.Close()
		return err
	}
	return nil
}

// registerDownload keeps download under handle, unless the profile was
// disposed, and reports whether it did. The profile's entry and the owner
// index change in one hold of p.mu, here and in releaseDownload.
func (p *webkitProfile) registerDownload(handle string, download unsafe.Pointer) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disposed {
		return false
	}
	p.downloads[handle] = download
	webkitDownloadOwners.Store(handle, p)
	return true
}

func (p *webkitProfile) isDisposed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disposed
}

func (p *webkitProfile) releaseDownload(handle string) {
	p.mu.Lock()
	download := p.downloads[handle]
	delete(p.downloads, handle)
	webkitDownloadOwners.CompareAndDelete(handle, p)
	p.mu.Unlock()
	if download != nil {
		webkitReleaseDownload(download)
	}
}

// heldDownload answers the download registered under id, nil once it is
// released.
func (p *webkitProfile) heldDownload(id string) unsafe.Pointer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.downloads[id]
}

// CancelDownload looks the download up on the GTK thread, where a released
// download's reference is dropped after it leaves downloads
// (releaseDownload), so the one it finds is still held through the cancel.
//
// A download released first has finished or failed, and its report is on
// its way (aoWebKitDownloadProgress releases before it reports).
func (p *webkitProfile) CancelDownload(id string) error {
	return webkitCancelDownload(p, id).err(errGTKUnavailable)
}

func (p *webkitProfile) Dispose(context.Context) error {
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return nil
	}
	p.disposed = true
	pages := make([]*webkitPage, 0, len(p.pages))
	for page := range p.pages {
		pages = append(pages, page)
	}
	p.pages = make(map[*webkitPage]struct{})
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
	// Before the session goes: a destination still being decided is
	// canceled and released on the GTK thread, where its worker's
	// completion would have taken it.
	webkitCancelDestinations(p)
	webkitProfileByID.Delete(p.id)
	p.engine.mu.Lock()
	delete(p.engine.profiles, p.id)
	p.engine.mu.Unlock()
	webkitFreeSession(p.session)
	return nil
}

// webkitDownloadOwners maps a download handle to the profile that started it,
// because a progress report arrives from a C signal with no Go receiver of its
// own. A report for a disposed profile is a miss, not a routed event.
var webkitDownloadOwners sync.Map

func webkitDownloadOwner(handle string) *webkitProfile {
	if value, ok := webkitDownloadOwners.Load(handle); ok {
		return value.(*webkitProfile)
	}
	return nil
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
