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
	"agent-overflow/internal/browser/ids"
)

// The WebKitGTK engine's profile half (spec §6): the WebKitNetworkSession a
// profile owns, the pages it makes and adopts, and the downloads it holds.
// The locking rule in webkit_engine_linux.go holds here.

// downloadStart names where a download of this profile began: page, its
// view's page, or, for a popup not adopted yet, the popup's number. A
// popup's download names the popup by its handle and its opener, before
// adoption and after, so the Manager lists it while the popup is not
// (popup_parked.go).
func (p *webkitProfile) downloadStart(page *webkitPage, popup uint64) downloadStart {
	var start downloadStart
	switch {
	case page != nil:
		// The page is its only frame (OwnsFrame).
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
		engine: e, id: id, handle: profileHandle(fmt.Sprintf("profile-%d", id)), session: session,
		stage: opts.StageDownload, releaseStaged: opts.ReleaseStaged,
		destinations: destination.NewDecisions[downloadID, webkitPendingDownload](),
		pages:        make(map[*webkitPage]struct{}), downloads: make(map[downloadID]unsafe.Pointer),
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
	handle  profileHandle
	session unsafe.Pointer

	// stage names the staged file each download is written to
	// (profileOptions.StageDownload), and releaseStaged gives up one no
	// download start took.
	stage         func(id downloadID) (string, error)
	releaseStaged func(id downloadID)
	// destinations are the downloads whose destination decision is open:
	// held on the GTK thread as WebKit hands it over, taken there once by
	// its completion or by Dispose.
	destinations *destination.Decisions[downloadID, webkitPendingDownload]

	mu        sync.Mutex
	disposed  bool
	pages     map[*webkitPage]struct{}
	downloads map[downloadID]unsafe.Pointer

	// beforeView, when set, runs between a new page's shell and its view's
	// construction: a test seam.
	beforeView func()
}

func (p *webkitProfile) Handle() profileHandle { return p.handle }

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

func (p *webkitProfile) AttachPage(_ context.Context, handle ids.PageHandle, hooks pageHooks) (pageDriver, error) {
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
func (p *webkitProfile) registerDownload(handle downloadID, download unsafe.Pointer) bool {
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

func (p *webkitProfile) releaseDownload(handle downloadID) {
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
func (p *webkitProfile) heldDownload(id downloadID) unsafe.Pointer {
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
func (p *webkitProfile) CancelDownload(id downloadID) error {
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
	handles := make([]downloadID, 0, len(p.downloads))
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

func webkitDownloadOwner(handle downloadID) *webkitProfile {
	if value, ok := webkitDownloadOwners.Load(handle); ok {
		return value.(*webkitProfile)
	}
	return nil
}
