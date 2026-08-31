//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"
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
// `gtkDo` waits for the GTK main thread, and that thread may be running a
// delegate — a popup, a navigation-authority answer — that wants the very lock
// being held. So a lock here only ever covers map bookkeeping, and every
// gtkDo-backed call happens after it is released.

const (
	// A background page still needs a real viewport: layout, media queries,
	// and element geometry are all read from it, and a screenshot of a hidden
	// page is taken at this size. It is the size the pane opens at, so
	// presenting a page does not reflow everything it has already measured.
	webkitHiddenWidth  = 1280
	webkitHiddenHeight = 800
)

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
		popups: make(map[string]unsafe.Pointer), slots: make(map[int]bool),
		pane: make(map[uint64]webkitPaneState),
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

	mu        sync.Mutex
	popupSeq  uint64
	popups    map[string]unsafe.Pointer
	slots     map[int]bool
	profiles  map[uint64]*webkitProfile
	pane      map[uint64]webkitPaneState
	stopped   bool
	pageCount int
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
	for handle, view := range e.popups {
		orphans = append(orphans, view)
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

// holdPopup takes ownership of an engine-created view until the Manager adopts
// or discards it. The engine never decides which.
func (e *webkitEngine) holdPopup(view unsafe.Pointer) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.popupSeq++
	handle := fmt.Sprintf("popup-%d", e.popupSeq)
	e.popups[handle] = view
	return handle
}

func (e *webkitEngine) takePopup(handle string) unsafe.Pointer {
	e.mu.Lock()
	defer e.mu.Unlock()
	view := e.popups[handle]
	delete(e.popups, handle)
	return view
}

func (e *webkitEngine) DiscardPage(handle string) {
	if view := e.takePopup(handle); view != nil {
		webkitCloseView(view)
	}
}

// claimSlot reserves one row in the background host. Parked views are stacked
// by slot inside a 1x1 clipping scroller, so two pages must never share one.
func (e *webkitEngine) claimSlot() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for slot := 0; ; slot++ {
		if !e.slots[slot] {
			e.slots[slot] = true
			return slot
		}
	}
}

func (e *webkitEngine) releaseSlot(slot int) {
	e.mu.Lock()
	delete(e.slots, slot)
	e.mu.Unlock()
}

func (e *webkitEngine) NewProfile(_ context.Context, opts profileOptions) (engineProfile, error) {
	if !e.Running() {
		return nil, fmt.Errorf("browser: engine unavailable")
	}
	id := webkitProfileSeq.Add(1)
	// The workspace digest, not its path: a workspace root can be long, can
	// hold characters a directory name cannot, and must not be readable from
	// the profile directory listing.
	digest := sha256.Sum256([]byte(opts.Workspace))
	// browserProfileDir is the same constant ClearSiteData deletes: the two
	// must name one tree or clearing site data would miss the engine's.
	root := filepath.Join(e.configDir, browserProfileDir, fmt.Sprintf("%x", digest[:12]))
	dataDir := filepath.Join(root, "data")
	cacheDir := filepath.Join(root, "cache")
	cookieFile := filepath.Join(dataDir, "cookies.sqlite")
	if opts.Persist {
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
	session, err := webkitNewSession(dataDir, cacheDir, cookieFile, opts.DownloadDir, !opts.Persist, id)
	if err != nil {
		return nil, err
	}
	profile := &webkitProfile{
		engine: e, id: id, handle: fmt.Sprintf("profile-%d", id), session: session,
		pages: make(map[*webkitPage]struct{}), downloads: make(map[string]unsafe.Pointer),
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

// webkitProfile is one workspace's WebKitNetworkSession: its own cookie jar,
// cache, and download destination, never the process-wide default session
// (which would write into the user's ~/.local/share/webkitgtk).
type webkitProfile struct {
	engine  *webkitEngine
	id      uint64
	handle  string
	session unsafe.Pointer

	mu        sync.Mutex
	disposed  bool
	pages     map[*webkitPage]struct{}
	downloads map[string]unsafe.Pointer
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
	page, err := p.newPageShell(hooks)
	if err != nil {
		return nil, err
	}
	view, err := webkitNewView(p.session, page.id, script, webkitConsoleHandler)
	if err != nil {
		page.discard()
		return nil, err
	}
	page.view = view
	p.parkNew(page)
	return page, nil
}

func (p *webkitProfile) AttachPage(_ context.Context, handle string, hooks pageHooks) (pageDriver, error) {
	view := p.engine.takePopup(handle)
	if view == nil {
		return nil, fmt.Errorf("browser: page %s is no longer available", handle)
	}
	script := webkitUserScript()
	page, err := p.newPageShell(hooks)
	if err != nil {
		webkitCloseView(view)
		return nil, err
	}
	page.view = view
	// The popup already exists and may already be loading, so its document
	// start is gone: the injected script applies from its next document on.
	// That is the same trade CDP's addScriptToEvaluateOnNewDocument makes for
	// an adopted target.
	if err := webkitAdoptView(view, page.id, script, webkitConsoleHandler); err != nil {
		page.discard()
		webkitCloseView(view)
		return nil, err
	}
	p.parkNew(page)
	return page, nil
}

// newPageShell allocates the Go half of a page and registers it BEFORE the view
// exists, so a delegate that fires during construction resolves to a real page
// instead of being dropped.
func (p *webkitProfile) newPageShell(hooks pageHooks) (*webkitPage, error) {
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return nil, fmt.Errorf("browser: workspace profile is closed")
	}
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	page := &webkitPage{
		engine: p.engine, profile: p, id: webkitPageSeq.Add(1),
		hooks: hooks, ctx: ctx, cancel: cancel, slot: -1,
	}
	page.handle = fmt.Sprintf("page-%d", page.id)
	webkitPageByID.Store(page.id, page)
	p.mu.Lock()
	p.pages[page] = struct{}{}
	p.mu.Unlock()
	return page, nil
}

// parkNew gives a page its slot in the background host. Parked is MAPPED: the
// view has a real viewport and a fresh snapshot, it simply costs the window no
// space. Hiding by opacity is banned — it stops rAF, which stops the page.
func (p *webkitProfile) parkNew(page *webkitPage) {
	page.slot = p.engine.claimSlot()
	webkitParkView(page.view, page.slot, webkitHiddenWidth, webkitHiddenHeight)
}

func (p *webkitProfile) registerDownload(handle string, download unsafe.Pointer) {
	p.mu.Lock()
	p.downloads[handle] = download
	p.mu.Unlock()
	webkitDownloadOwners.Store(handle, p)
}

func (p *webkitProfile) releaseDownload(handle string) {
	p.mu.Lock()
	download := p.downloads[handle]
	delete(p.downloads, handle)
	p.mu.Unlock()
	webkitDownloadOwners.Delete(handle)
	if download != nil {
		webkitReleaseDownload(download)
	}
}

func (p *webkitProfile) CancelDownload(id string) {
	p.mu.Lock()
	download := p.downloads[id]
	p.mu.Unlock()
	if download != nil {
		webkitCancelDownload(download)
	}
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
