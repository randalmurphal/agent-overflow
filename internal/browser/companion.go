package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/paneplace"

	"github.com/google/uuid"
)

const (
	// The viewport bounds `browser_viewport` accepts, and the box every
	// pointer coordinate must fall inside.
	minCompanionWidth  = 320
	minCompanionHeight = 240
	maxCompanionWidth  = 1920
	maxCompanionHeight = 1200
	// maxCompanionPanes bounds the mounted pane surfaces one process tracks,
	// so a client that never detaches cannot grow the registry without end.
	maxCompanionPanes = 64
)

// PaneRect is where a mounted browser pane's host rect sits, in the SPA's own
// CSS pixels, together with the SPA viewport it was measured in. A host never
// assumes CSS pixels equal its units: it scales the rect by the SPA's
// DevicePixelRatio where it positions in device pixels (the Windows
// launcher), or by its own size over the viewport, which makes the position
// exact under webview zoom (Ctrl+=) and any DPI.
// Visible is false while the pane is mounted but must not be painted over: an
// AO overlay intersects the rect, or the rect is entirely off the pane strip.
//
// Clip* is the VISIBLE intersection of the rect with every clipping ancestor,
// in the same CSS-pixel space. A native view cannot be cropped by the DOM, so
// the engine crops it: the view keeps the full rect's size (the page must not
// relayout because it scrolled half behind the sidebar) and the host clips its
// presentation to the clip rect. An unclipped pane reports clip == rect.
// Background is the pane surface's resolved CSS color ("#rrggbb"); engines
// paint it where the page has not presented yet, so freshly exposed strips
// match the pane instead of flashing the engine default.
//
// DevicePixelRatio is the SPA's window.devicePixelRatio: OS scale times
// webview zoom. An engine whose page renders at the OS scale alone (the
// hosted WebView2) divides by the page's own ratio to recover the zoom.
// Zero means unknown, which every consumer treats as 1.
type PaneRect struct {
	X                float64 `json:"x"`
	Y                float64 `json:"y"`
	Width            float64 `json:"width"`
	Height           float64 `json:"height"`
	ClipX            float64 `json:"clipX"`
	ClipY            float64 `json:"clipY"`
	ClipWidth        float64 `json:"clipWidth"`
	ClipHeight       float64 `json:"clipHeight"`
	ViewportWidth    float64 `json:"viewportWidth"`
	ViewportHeight   float64 `json:"viewportHeight"`
	DevicePixelRatio float64 `json:"devicePixelRatio,omitempty"`
	Visible          bool    `json:"visible"`
	Background       string  `json:"background,omitempty"`
	// Seq is the SPA's per-mount number for this report, advanced once per
	// distinct report. The Windows SPA posts the same report to the launcher
	// under the same number, and the hosted engine's bounds directive
	// carries it, so the launcher places from whichever is newer. Zero is a
	// reporter that does not number (tests, the harness bridge).
	Seq uint64 `json:"seq,omitempty"`
}

// PanePlacement is where a presented page's view goes: the page keeps its
// own viewport (PageWidth x PageHeight, the size the agent works against)
// and the pane shows it scaled by Scale, never resized by the placement. A
// page following the pane is its size and Scale is 1; a pinned page larger
// than the pane scales down. Rect is the FITTED rect in the SPA's CSS pixels
// (paneplace.Fit), with its clip already intersected with the host rect's
// visible clip. Rect's viewport, background and device pixel ratio are the
// host rect's own. Mount is the pane mount whose rect it was placed in, which
// lets a host that measures the mount itself place the page from its own,
// newer rect (the Windows launcher).
type PanePlacement struct {
	Rect       PaneRect
	PageWidth  int
	PageHeight int
	Scale      float64
	Mount      string
}

// placePage fits a page of pageW x pageH CSS pixels inside a host rect with
// the shared placement rule (paneplace.Fit). ok is false when nothing of the
// page would be visible, which the caller treats as hidden.
func placePage(rect PaneRect, pageW, pageH int) (PanePlacement, bool) {
	fit, scale, ok := paneplace.Fit(paneplace.Rect{
		X: rect.X, Y: rect.Y, W: rect.Width, H: rect.Height,
		ClipX: rect.ClipX, ClipY: rect.ClipY, ClipW: rect.ClipWidth, ClipH: rect.ClipHeight,
	}, pageW, pageH)
	if !ok {
		return PanePlacement{}, false
	}
	fitted := rect
	fitted.X, fitted.Y, fitted.Width, fitted.Height = fit.X, fit.Y, fit.W, fit.H
	fitted.ClipX, fitted.ClipY, fitted.ClipWidth, fitted.ClipHeight = fit.ClipX, fit.ClipY, fit.ClipW, fit.ClipH
	return PanePlacement{Rect: fitted, PageWidth: pageW, PageHeight: pageH, Scale: scale}, true
}

// overlayPaneLayout resolves a placement for a host that positions in whole
// pixels of an overlay covering the SPA window (the WebKitGTK pane overlay):
// the CSS-to-overlay proportion is the overlay's size over the SPA viewport,
// so it carries DPI and webview zoom without either side knowing them; no
// viewport means the rect is already in overlay pixels. The edges snap with
// paneplace.Layout. The answered scale draws the page's CSS pixels at the
// placement's scale in overlay pixels.
func overlayPaneLayout(placement PanePlacement, overlayW, overlayH int) (paneplace.HostLayout, float64) {
	rect := placement.Rect
	sx, sy := 1.0, 1.0
	if rect.ViewportWidth > 0 && overlayW > 0 {
		sx = float64(overlayW) / rect.ViewportWidth
	}
	if rect.ViewportHeight > 0 && overlayH > 0 {
		sy = float64(overlayH) / rect.ViewportHeight
	}
	scale := placement.Scale
	if scale <= 0 {
		scale = 1
	}
	layout := paneplace.Layout(paneplace.Rect{
		X: rect.X, Y: rect.Y, W: rect.Width, H: rect.Height,
		ClipX: rect.ClipX, ClipY: rect.ClipY, ClipW: rect.ClipWidth, ClipH: rect.ClipHeight,
	}, sx, sy)
	return layout, scale * sx
}

// paneMount is one mounted pane surface: the frontend's claim that a host rect
// for this thread exists, plus the last rect it reported. The mount is the
// unit connection cleanup releases, so a dead UI can never leave a native view
// painted over a window that no longer renders the pane under it.
type paneMount struct {
	threadID string
	rect     PaneRect
	hasRect  bool
	// seq is the mount's place in attach order, which decides between two
	// mounts that could both present the thread's page (presenterLocked).
	seq uint64
	// findSeq is the sequence of the newest find start the Manager took
	// from this mount (admitFindStart).
	findSeq uint64
	// focus is the mount's page focus intent (focus_intent.go).
	focus *mountFocus
}

// A thread's state push shows each of its tabs' info, loading state and
// favicon. Every write that changes one of them sets the page's unpushed
// record (pageLoad.unpushed, under metaMu), and only a push to every client
// clears it (buildThreadState), taking it before it reads the tabs, so a
// write that lands in between sets it again. publishPages pushes a thread
// whose pages hold one: a write is never lost whoever publishes next, and an
// operation that changed nothing pushes nothing.

// A page's info has two kinds of writer. An engine report (pageInfoReport)
// carries the engine's address and title as they changed, numbered in the
// engine's order; it is committed unless a newer report already was. A read
// asks the engine and commits what it was answered (commitRead). The address
// and the history state are fenced apart: each holds the stamp of the write
// that set it (readStamp, or a stamp taken at the commit for any other
// write), and a read's part is committed only if no write that began after
// the read set that field. A read run for an operation holds the
// page-operation lock from the read through its commit, so no other
// operation's read lands in between.

// pageUpdate is one write of a page's cached info. commitInfo applies it,
// and nothing else changes the info of a page once it exists.
type pageUpdate struct {
	// Location is the address and title; nil keeps them (a history refresh,
	// a label).
	Location *pageLocation
	// History is the back/forward state; nil when the source does not know
	// it (an engine report, a snapshot script), and the tab keeps the state
	// it shows rather than turn its buttons off.
	History *pageHistory
	// Listing is what only a browser_pages answer means; a committed
	// Location replaces it, so any other read clears it.
	Listing pageListing
	// Label, when set, replaces the label; nil keeps it.
	Label *string
	// Report is an engine report's order (pageHooks.Info), zero for any
	// other write. A report is committed only if newer than every report
	// committed before it.
	Report uint64
	// Stamp, for a read's answer, is the stamp the read took before it
	// asked (readStamp): each part is committed only if its field was not
	// set by a write that began later. Zero takes a stamp at the commit,
	// newer than every read begun: an engine report, a label, or a page's
	// own write before it is listed (openAndLoad).
	Stamp uint64
}

type pageLocation struct{ URL, Title string }

type pageHistory struct{ CanGoBack, CanGoForward bool }

// pageListing is PageInfo.Selected and LastOpened, which each browser_pages
// call rewrites and no tab shows.
type pageListing struct {
	Selected   bool
	LastOpened string
}

// commitInfo applies the parts of u that are newer than what the page holds
// (Report, Stamp), bounding the address and title, and sets the page's
// unpushed record when what the tab shows changed. It answers the page's
// info after, and whether the address u carries was committed.
func (p *managedPage) commitInfo(u pageUpdate) (PageInfo, bool) {
	p.metaMu.Lock()
	defer p.metaMu.Unlock()
	if u.Report != 0 {
		if u.Report <= p.infoReport {
			return p.info, false
		}
		p.infoReport = u.Report
	}
	stamp := u.Stamp
	if stamp == 0 {
		p.infoStamp++
		stamp = p.infoStamp
	}
	info := p.info
	located := false
	if u.Location != nil && stamp > p.locationStamp {
		info.URL = truncateUTF8(u.Location.URL, maxBrowserURLBytes)
		info.Title = truncateUTF8(u.Location.Title, maxBrowserTitleBytes)
		info.Selected, info.LastOpened = u.Listing.Selected, u.Listing.LastOpened
		p.locationStamp = stamp
		located = true
	}
	if u.History != nil && stamp > p.historyStamp {
		info.CanGoBack, info.CanGoForward = u.History.CanGoBack, u.History.CanGoForward
		p.historyStamp = stamp
	}
	if u.Label != nil {
		info.Label = *u.Label
	}
	if pushedInfo(info) != pushedInfo(p.info) {
		p.load.unpushed = true
	}
	p.info = info
	return info, located
}

// readStamp answers a stamp newer than every write committed so far; a read
// takes one before it asks the engine.
func (p *managedPage) readStamp() uint64 {
	p.metaMu.Lock()
	defer p.metaMu.Unlock()
	p.infoStamp++
	return p.infoStamp
}

// commitRead commits u, the answer of a read that took stamp before it
// asked (commitInfo).
func (m *Manager) commitRead(p *managedPage, u pageUpdate, stamp uint64) (PageInfo, bool) {
	if m.infoRead != nil {
		m.infoRead(p)
	}
	u.Stamp = stamp
	return p.commitInfo(u)
}

// pushedInfo is info without its listing (pageListing).
func pushedInfo(info PageInfo) PageInfo {
	info.Selected, info.LastOpened = false, ""
	return info
}

func (p *managedPage) cachedInfo() PageInfo {
	p.metaMu.RLock()
	info := p.info
	p.metaMu.RUnlock()
	return info
}

// hasUnpushed reports whether a write to p is not in a push yet.
func (p *managedPage) hasUnpushed() bool {
	p.metaMu.RLock()
	defer p.metaMu.RUnlock()
	return p.load.unpushed
}

// threadPagesLocked answers the thread's pages that have a managedPage: the
// ones being created and the live ones. The caller holds m.mu.
func (m *Manager) threadPagesLocked(threadID string) []*managedPage {
	var pages []*managedPage
	for _, pending := range m.pending {
		if pending.p.owner == threadID && !pending.closed {
			pages = append(pages, pending.p)
		}
	}
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == threadID {
				pages = append(pages, p)
			}
		}
	}
	return pages
}

// publishPages pushes the thread's state when a write to one of its pages
// is not in a push yet.
func (m *Manager) publishPages(threadID string) {
	m.mu.Lock()
	unpushed := false
	for _, p := range m.threadPagesLocked(threadID) {
		if p.hasUnpushed() {
			unpushed = true
			break
		}
	}
	m.mu.Unlock()
	if unpushed {
		m.emitThreadState(threadID)
	}
}

// pageAdded follows p joining its thread's tabs (created, adopted or
// restored): a use, the thread's active page when it has none, and a push.
func (m *Manager) pageAdded(p *managedPage) {
	p.touch()
	m.ensureActivePage(p.owner, p.id)
	m.emitThreadState(p.owner)
	m.syncPanePresentation(p.owner)
}

// pageUsed follows an operation on p: a use, the thread's active page when it
// has none, and a push only when that or a write is not in a push yet.
func (m *Manager) pageUsed(p *managedPage) {
	p.touch()
	if m.ensureActivePage(p.owner, p.id) {
		m.emitThreadState(p.owner)
	} else {
		m.publishPages(p.owner)
	}
	m.syncPanePresentation(p.owner)
}

// threadState is the thread's state for one client's read (an attach, a
// call's answer). It leaves the unpushed records for the next push.
func (m *Manager) threadState(threadID string) CompanionEvent {
	return m.buildThreadState(threadID, false)
}

// buildThreadState builds the thread's state. For a push to every client
// (push) it first takes the unpushed records of the thread's pages.
func (m *Manager) buildThreadState(threadID string, push bool) CompanionEvent {
	m.mu.Lock()
	if push {
		for _, p := range m.threadPagesLocked(threadID) {
			p.metaMu.Lock()
			p.load.unpushed = false
			p.metaMu.Unlock()
		}
	}
	tabs := m.companionTabsLocked(threadID)
	if push && m.pushCopied != nil {
		m.pushCopied()
	}
	session, hasSession := m.sessions[threadID]
	active, _ := m.livePageLocked(session.ActivePageID)
	downloads := m.threadDownloadsLocked(threadID)
	m.stateSeq++
	seq := m.stateSeq
	recentGen := m.recentGen
	m.mu.Unlock()
	event := CompanionEvent{Kind: "state", ThreadID: threadID, Pages: make([]PageInfo, 0, len(tabs)), Downloads: downloads, Seq: seq}
	if active != nil && active.owner == threadID {
		event.FindMode = findModeOf(active)
	}
	visible := false
	if hasSession {
		visible = session.Visible
		event.SessionName = session.Name
	}
	event.Visible = &visible
	for _, tab := range tabs {
		event.Pages = append(event.Pages, tab.info)
	}
	event.ActivePageID = session.ActivePageID
	event.ViewportWidth, event.ViewportHeight = sessionViewport(session)
	event.ViewportSet = session.ViewportSet
	event.Jar = jarKindOf(session)
	event.RecentGen = recentGen
	return event
}

func (m *Manager) emit(event CompanionEvent) {
	m.mu.Lock()
	sink := m.eventSink
	m.mu.Unlock()
	if sink != nil {
		sink(event)
	}
}

// emitThreadState pushes the thread's state to every client.
func (m *Manager) emitThreadState(threadID string) {
	if strings.TrimSpace(threadID) != "" {
		m.emit(m.buildThreadState(threadID, true))
	}
}

// pageByHandleLocked resolves an engine handle to the page it drives. Caller
// holds m.mu.
func (m *Manager) pageByHandleLocked(handle string) *managedPage {
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.driver.Handle() == handle {
				return p
			}
		}
	}
	return nil
}

// creatingPageByHandleLocked returns the page being created or restored
// whose engine page has handle, or nil. The caller holds m.mu.
func (m *Manager) creatingPageByHandleLocked(handle string) *managedPage {
	for _, scope := range m.scopes {
		for p := range scope.creating {
			if p.driver != nil && p.driver.Handle() == handle {
				return p
			}
		}
	}
	return nil
}

// pageListedLocked reports whether p is in its thread's tabs. The caller
// holds m.mu.
func (m *Manager) pageListedLocked(p *managedPage) bool {
	for _, scope := range m.scopes {
		if scope.pages[p.id] == p {
			return true
		}
	}
	return false
}

// pageEscape is the key the page always keeps: a bare Escape, which AO only
// observes (keyChord).
var pageEscape = keybindings.Accelerator{Key: "escape"}

// pageEscapeAnnounced decides whether a key pressed in a page is announced
// as the page's Escape: a bare Escape, newly pressed (not repeating, not
// held), that no input method composition takes to cancel itself.
func pageEscapeAnnounced(pressed keybindings.Accelerator, press KeyPress) bool {
	return pressed == pageEscape && !press.Repeat && !press.Held && !press.Composing
}

// keyChord is engineEvents.KeyChord: it answers on the engine's UI thread, so
// it is one set lookup, and the routing that takes m.mu happens off it.
//
// A bare Escape is never AO's: the page has it exactly as if AO were not
// there. When pageEscapeAnnounced says so it is announced (emitEscape), so
// a find bar the pane shows over the page can close.
//
// A chord AO claims takes keyboard focus out of the page: the command it
// runs puts focus where it wants it (a find bar's input, the palette), and
// the keys typed next must reach that, not the page. So every claim hands
// focus to the app (engineAppFocus) before it returns; a chord AO does not
// claim leaves focus in the page.
func (m *Manager) keyChord(handle string, pressed keybindings.Accelerator, press KeyPress) bool {
	if pressed == pageEscape {
		if pageEscapeAnnounced(pressed, press) {
			go m.emitEscape(handle)
		}
		return false
	}
	if m.accelerators == nil {
		return false
	}
	bound, ok := m.accelerators().Match(pressed)
	if !ok {
		return false
	}
	if engine, ok := m.engine.(engineAppFocus); ok {
		if err := engine.FocusApp(); err != nil {
			// The chord still runs; its surface opens with focus left in
			// the page, as before the handoff existed.
			log.Printf("browser: hand keyboard focus to the app for %s: %v", handle, err)
		}
	}
	go m.emitAccelerator(handle, bound)
	return true
}

// emitEscape announces a bare Escape pressed in a page a pane shows, as a
// Kind "escape" event naming the thread, the page and the presenting mount,
// whose client's find bar it closes.
func (m *Manager) emitEscape(handle string) {
	m.mu.Lock()
	p := m.pageByHandleLocked(handle)
	mount := ""
	if p != nil {
		mount, _, _ = m.presenterLocked(p.owner)
	}
	m.mu.Unlock()
	if p == nil || mount == "" || !m.pageShown(p.owner, p.id) {
		return
	}
	m.emit(CompanionEvent{Kind: "escape", ThreadID: p.owner, PageID: p.id, Mount: mount})
}

// emitAccelerator routes a claimed chord to the pane mount showing the page,
// so that only the client holding the mount replays it. A page no pane shows
// has nobody to act for, and its chord is dropped.
func (m *Manager) emitAccelerator(handle string, bound keybindings.Accelerator) {
	m.mu.Lock()
	p := m.pageByHandleLocked(handle)
	mount := ""
	if p != nil && m.presentedPageLocked(p.owner) == p.id {
		mount, _, _ = m.presenterLocked(p.owner)
	}
	m.mu.Unlock()
	if p == nil {
		return
	}
	if mount == "" {
		log.Printf("browser: drop the chord %+v pressed in page %s: no pane shows the page", bound, p.id)
		return
	}
	m.emit(CompanionEvent{Kind: "accelerator", ThreadID: p.owner, Mount: mount, Accelerator: &bound})
}

// AcceleratorsChanged tells an engine that matches chords out of process
// (engineAccelerators) that the bound set it holds is stale.
func (m *Manager) AcceleratorsChanged() {
	if engine, ok := m.engine.(engineAccelerators); ok {
		engine.SyncAccelerators()
	}
}

// updatePageInfo is engineEvents.PageInfoChanged, a CDP engine's report of
// a page by its handle.
func (m *Manager) updatePageInfo(handle, url, title string, order uint64) {
	m.mu.Lock()
	found := m.pageByHandleLocked(handle)
	if found == nil {
		found = m.creatingPageByHandleLocked(handle)
	}
	m.mu.Unlock()
	if found != nil {
		m.pageInfoReport(found, url, title, order)
	}
}

// pageInfoReport is pageHooks.Info: an engine report of p's address and
// title. It is committed to p wherever p is, so the report order fence sees
// every report from the moment p's engine page exists: a page being created,
// restored or adopted takes it into its unlisted info, which its own load
// read replaces when that read began later, and a page that left its
// thread's tabs takes it into info nothing reads again. Only a listed page
// pushes it and refreshes its history state; an unlisted page's load read
// carries the history and its listing pushes.
func (m *Manager) pageInfoReport(p *managedPage, url, title string, order uint64) {
	if _, committed := p.commitInfo(pageUpdate{Location: &pageLocation{URL: url, Title: title}, Report: order}); !committed {
		// A newer report was committed first and runs its own refresh.
		return
	}
	m.mu.Lock()
	listed := m.pageListedLocked(p)
	m.mu.Unlock()
	if !listed {
		return
	}
	stamp := p.readStamp()
	// An engine reports the same address and title again (a target info
	// change that touched neither); that alone pushes nothing.
	m.publishPages(p.owner)
	go m.refreshHistoryState(p, stamp)
}

// refreshHistoryState re-reads one page's back/forward availability after an
// engine announced a navigation, off the engine's event goroutine — the read
// is a driver round trip, and blocking the event dispatcher on it could
// deadlock an engine whose events and commands share a loop. It runs without
// the page-operation lock under the stamp taken after the report (stamp): an
// operation's read that began later carries the history state itself and
// stands, and an address commit does not touch its answer.
func (m *Manager) refreshHistoryState(p *managedPage, stamp uint64) {
	ctx, cancel := operationContext(context.Background(), p.driver.Lifetime(), operationTimeout)
	defer cancel()
	back, forward, err := p.driver.HistoryState(ctx)
	if err != nil {
		return
	}
	m.commitRead(p, pageUpdate{History: &pageHistory{CanGoBack: back, CanGoForward: forward}}, stamp)
	m.publishPages(p.owner)
}

func (m *Manager) CompanionState(access Access) CompanionEvent {
	return m.threadState(access.ThreadID)
}

// AttachPane registers one mounted pane surface for a thread and answers the
// state snapshot the pane renders its chrome from. The returned id is what
// SetPaneRect addresses and what DetachPane (or connection cleanup) releases.
func (m *Manager) AttachPane(access Access) (CompanionSubscription, error) {
	state := m.threadState(access.ThreadID)
	if len(state.Pages) == 0 {
		return CompanionSubscription{}, fmt.Errorf("browser: thread has no open pages")
	}
	id := uuid.NewString()
	m.mu.Lock()
	if len(m.panes) >= maxCompanionPanes {
		m.mu.Unlock()
		return CompanionSubscription{}, fmt.Errorf("browser: too many mounted browser panes")
	}
	m.paneSeq++
	m.panes[id] = paneMount{threadID: access.ThreadID, seq: m.paneSeq}
	m.mu.Unlock()
	return CompanionSubscription{ID: id, State: state}, nil
}

// DetachPane releases one pane mount. The presentation sync runs so an engine
// with a presented native view hides it rather than painting over whatever
// replaced the pane. A mount that remains for the thread becomes the
// presenter if this one was it (presenterLocked) and hands the pages its
// own size.
func (m *Manager) DetachPane(id string) {
	m.mu.Lock()
	mount, ok := m.panes[id]
	resize := false
	if ok {
		delete(m.panes, id)
		// Unmounting is a focus choice: a request in flight must not land.
		if mount.focus != nil && mount.focus.intent != nil {
			mount.focus.intent.withdrawn.Store(true)
		}
		if next, _, _ := m.presenterLocked(mount.threadID); next != "" {
			if other := m.panes[next]; other.hasRect {
				resize = m.notePaneSizeLocked(other.threadID, other.rect)
			}
		}
	}
	m.mu.Unlock()
	if ok {
		if resize {
			m.scheduleViewport(mount.threadID)
		}
		m.syncPanePresentation(mount.threadID)
	}
}

// notePaneSizeLocked records a mounted pane's size on its thread's session
// and reports whether the pages should take it: the size changed and no
// viewport is pinned. Caller holds m.mu.
func (m *Manager) notePaneSizeLocked(threadID string, rect PaneRect) bool {
	w, h, sized := paneViewportSize(rect)
	if !sized {
		return false
	}
	info := m.sessionLocked(threadID)
	if info.PaneW == w && info.PaneH == h {
		return false
	}
	info.PaneW, info.PaneH = w, h
	info.resolveViewport()
	info.UpdatedAt = time.Now()
	m.sessions[threadID] = info
	return !info.ViewportSet
}

// SetPaneRect records the mounted pane's current host rect. The frontend
// coalesces to one report per changed frame, so this path must stay cheap: a
// bookkeeping write, one presentation sync, and while no viewport is pinned a
// latest-wins request for the pages to take the rect's size (viewport.go).
func (m *Manager) SetPaneRect(id string, rect PaneRect) error {
	if rect.Width < 0 || rect.Height < 0 {
		rect.Width, rect.Height = 0, 0
	}
	if rect.ClipX == 0 && rect.ClipY == 0 && rect.ClipWidth == 0 && rect.ClipHeight == 0 {
		// A reporter that predates clipping (tests, the harness bridge) means
		// "unclipped", and downstream engines must never see a zero clip they
		// would crop everything away with.
		rect.ClipX, rect.ClipY = rect.X, rect.Y
		rect.ClipWidth, rect.ClipHeight = rect.Width, rect.Height
	}
	if rect.ClipWidth < 0 || rect.ClipHeight < 0 {
		rect.ClipWidth, rect.ClipHeight = 0, 0
	}
	m.mu.Lock()
	mount, ok := m.panes[strings.TrimSpace(id)]
	resize := false
	if ok {
		mount.rect = rect
		mount.hasRect = true
		m.panes[strings.TrimSpace(id)] = mount
		// The size is tracked whether or not the rect is paintable: a drag
		// hides the view and the page must already be at the new size when
		// the drop shows it again. Only the presenter's rect sizes the pages.
		if presenter, _, _ := m.presenterLocked(mount.threadID); presenter == strings.TrimSpace(id) {
			resize = m.notePaneSizeLocked(mount.threadID, rect)
		}
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("browser: pane mount not found")
	}
	if resize {
		m.scheduleViewport(mount.threadID)
	}
	m.syncPanePresentation(mount.threadID)
	return nil
}

// paneCanShow reports whether a mount's rect can show a page: reported,
// visible, and neither empty nor clipped away.
func paneCanShow(mount paneMount) bool {
	return mount.hasRect && mount.rect.Visible && mount.rect.Width >= 1 && mount.rect.Height >= 1 &&
		mount.rect.ClipWidth >= 1 && mount.rect.ClipHeight >= 1
}

// presenterLocked answers the one mount the thread's pages are presented in,
// its rect, and whether that rect can show a page now. A native view is
// placed at exactly one rect, so every reader asks this one rule and none
// chooses for itself: the presentation, the pages' size, a routed chord and
// a routed Escape. The presenter is the thread's earliest attached live
// mount. Attach order only grows, so the role stays with a mount until it
// detaches, through a hidden or off-screen spell (a drag, a compact screen),
// and a mount attached later never takes it. Only a mount the host window
// attached exists here (BrowserCompanionPaneAttach). The caller holds m.mu.
func (m *Manager) presenterLocked(threadID string) (string, PaneRect, bool) {
	id := ""
	var mount paneMount
	for candidate, other := range m.panes {
		if other.threadID == threadID && (id == "" || other.seq < mount.seq) {
			id, mount = candidate, other
		}
	}
	if id == "" {
		return "", PaneRect{}, false
	}
	return id, mount.rect, paneCanShow(mount)
}

// presentedPageLocked answers the page a mounted pane shows: the thread's
// active page while its companion is visible and a pane can show it, or "".
// The caller holds m.mu.
func (m *Manager) presentedPageLocked(threadID string) string {
	session, ok := m.sessions[threadID]
	if !ok || !session.Visible {
		return ""
	}
	_, rect, shown := m.presenterLocked(threadID)
	if !shown {
		return ""
	}
	pageW, pageH := sessionViewport(session)
	if _, ok := placePage(rect, pageW, pageH); !ok {
		return ""
	}
	return session.ActivePageID
}

// OpenPaneDevTools opens the engine's inspector for one of the thread's pages.
// Only engines with an inspector they can open implement paneDevTools;
// WKWebView (Safari's Develop menu is the inspector) and the fake engine
// answer with an explained refusal rather than a silent no-op.
func (m *Manager) OpenPaneDevTools(ctx context.Context, access Access, pageID string) error {
	host, ok := m.engine.(paneDevTools)
	if !ok {
		return fmt.Errorf("browser: devtools are not available on this browser engine")
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return err
	}
	host.OpenPageDevTools(p.driver.Handle())
	return nil
}

// syncPanePresentation decides WHICH of a thread's pages is presented and
// whether the pane is showing at all, then tells the engine the outcome. The
// decision itself stays here, in the Manager: an engine is told the outcome,
// never the rule.
//
// Where the pane SITS is the frontend's answer: the mounted pane reports its
// host rect (SetPaneRect) and that rect rides along, so bounds always land
// before the show. At most one pane per thread exists (a frontend invariant),
// and a mounted pane without a usable rect yet keeps the view hidden rather
// than flashing it at a stale place.
//
// An engine whose pages are not real windows implements no paneHost, so this
// is a type assertion that fails and costs nothing on every deployment that
// cannot present a page at all.
//
// A find runs on the shown active page only, so this is also where a find on
// a page that stopped being it (a tab switch, a hidden companion) ends, and
// so does a find bar whose pane mount is gone.
// Occlusion is not hiding: an overlay over the pane leaves the find running.
//
// Passes of a thread never overlap: each reads the state and tells the
// engine without a lock held, so two at once could land in either order and
// leave a page shown that the newer state hides. A call made while a pass
// runs leaves it to that caller, which runs one more pass after its own, so
// the last pass always reads the newest state and no caller waits on a pass
// it did not start (an engine's UI thread among them).
func (m *Manager) syncPanePresentation(threadID string) {
	m.mu.Lock()
	st := m.presenting[threadID]
	if st == nil {
		if m.presenting == nil {
			m.presenting = make(map[string]*presentSync)
		}
		st = &presentSync{}
		m.presenting[threadID] = st
	}
	st.dirty = true
	if st.busy {
		m.mu.Unlock()
		return
	}
	st.busy = true
	for st.dirty {
		st.dirty = false
		m.mu.Unlock()
		m.presentPanes(threadID)
		m.mu.Lock()
	}
	delete(m.presenting, threadID)
	m.mu.Unlock()
}

// presentSync is one thread's presentation passes: busy while one runs,
// dirty when another was asked for. Guarded by Manager.mu.
type presentSync struct{ busy, dirty bool }

// presentPanes is one presentation pass of syncPanePresentation.
//
// A page is never newly shown while it lags the thread's viewport (a pane
// resize lays out only the page shown then, viewport.go): it stays hidden and
// the drain lays it out and runs another pass. A page already shown stays
// shown through its own resize, whose drain is under way, and a page that
// refused the size is shown anyway, under the error that refusal raised.
func (m *Manager) presentPanes(threadID string) {
	host, ok := m.engine.(paneHost)
	m.mu.Lock()
	session, hasSession := m.sessions[threadID]
	mount, rect, shown := m.presenterLocked(threadID)
	visible := hasSession && session.Visible && shown
	previous := m.presented[threadID]
	var pages []*managedPage
	var active *managedPage
	var closing []*managedPage
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner != threadID {
				continue
			}
			pages = append(pages, p)
			if p.id == session.ActivePageID {
				active = p
			}
		}
		for _, c := range scope.closing {
			if c.p.owner == threadID {
				closing = append(closing, c.p)
			}
		}
	}
	m.mu.Unlock()
	m.endHiddenFinds(pages)
	if !ok {
		return
	}
	pageW, pageH := sessionViewport(session)
	// A closed page is hidden at once; its engine teardown may run a while.
	// Hiding a page whose driver has since closed is a no-op on every host.
	for _, p := range closing {
		host.HidePage(p.driver.Handle())
	}
	var placement PanePlacement
	if visible {
		// The page keeps the session's viewport; the pane shows it scaled to
		// fit. A pane too small or too occluded to show any of it presents
		// nothing rather than a sliver.
		placement, visible = placePage(rect, pageW, pageH)
		placement.Mount = mount
	}
	lagging := false
	if visible && active != nil && active.id != previous {
		want := packViewport(pageW, pageH)
		if active.viewport.Load() != want && active.viewportFailed.Load() != want {
			visible, lagging = false, true
		}
	}
	for _, p := range pages {
		if p == active && visible {
			continue
		}
		host.HidePage(p.driver.Handle())
	}
	if visible && active != nil {
		host.SetPageBounds(active.driver.Handle(), placement)
		host.ShowPage(active.driver.Handle())
	}
	m.mu.Lock()
	if m.presented == nil {
		m.presented = make(map[string]string)
	}
	if visible && active != nil {
		m.presented[threadID] = active.id
	} else {
		delete(m.presented, threadID)
	}
	delete(m.awaitingLayout, threadID)
	if lagging {
		want := packViewport(sessionViewport(m.sessions[threadID]))
		if active.viewport.Load() == want || active.viewportFailed.Load() == want {
			// Laid out since the check above, by an apply that found no
			// page awaiting it: this pass's loop runs another, and no drain
			// is owed.
			if st := m.presenting[threadID]; st != nil {
				st.dirty = true
			}
			lagging = false
		} else {
			if m.awaitingLayout == nil {
				m.awaitingLayout = make(map[string]string)
			}
			m.awaitingLayout[threadID] = active.id
		}
	}
	m.mu.Unlock()
	if lagging {
		m.scheduleViewport(threadID)
	}
	// A focus intent waits for its page's presentation, never ShowPage.
	m.applyFocusIntent(threadID)
}

// MoveCompanionPage places one of the thread's tabs, in any state, at
// index in tab order (clamped). Moving a tab is not a touch. The moved prefix
// is renumbered 1..n, and a page opened later keeps appending at the end
// because its creation-time key is always larger. A suspended page's new
// position is saved with it.
func (m *Manager) MoveCompanionPage(access Access, pageID string, index int) error {
	pageID = strings.TrimSpace(pageID)
	m.mu.Lock()
	tabs := m.companionTabsLocked(access.ThreadID)
	ordered := make([]threadTab, 0, len(tabs))
	var moved *threadTab
	for i := range tabs {
		if tabs[i].id == pageID {
			moved = &tabs[i]
		} else {
			ordered = append(ordered, tabs[i])
		}
	}
	if moved == nil {
		m.mu.Unlock()
		return errPageNotFound
	}
	index = max(0, min(index, len(ordered)))
	ordered = append(ordered[:index], append([]threadTab{*moved}, ordered[index:]...)...)
	savedMoved := false
	for i, tab := range ordered {
		if tab.live != nil {
			tab.live.tabOrder.Store(int64(i + 1))
		} else if tab.pending != nil {
			tab.pending.p.tabOrder.Store(int64(i + 1))
		} else if tab.rec.tabOrder != int64(i+1) {
			tab.rec.tabOrder = int64(i + 1)
			savedMoved = true
		}
	}
	m.mu.Unlock()
	m.emitThreadState(access.ThreadID)
	if savedMoved {
		return m.persistThreads([]string{access.ThreadID})
	}
	return nil
}

// NewCompanionPage is the user opening a tab: it is selected and the
// companion shown while the engine page is created, and a failed creation
// stays as the tab's error (createPage). A non-empty address is then
// navigated as the address bar would. It answers the new page, even when its
// navigation failed; an empty ID with no error when the tab was closed while
// it was created or shows why it could not be.
func (m *Manager) NewCompanionPage(ctx context.Context, access Access, address string) (PageInfo, error) {
	if strings.TrimSpace(access.ThreadID) == "" || strings.TrimSpace(access.Workspace) == "" {
		return PageInfo{}, fmt.Errorf("browser: invalid caller scope")
	}
	p, err := m.createPage(ctx, access, true)
	var onTab *shownOnTab
	if errors.Is(err, errClosedWhileCreating) || errors.As(err, &onTab) {
		return PageInfo{}, nil
	}
	if err != nil {
		return PageInfo{}, err
	}
	if strings.TrimSpace(address) != "" {
		if _, err := m.NavigateCompanion(ctx, access, p.id, address); err != nil {
			return p.cachedInfo(), err
		}
	}
	return p.cachedInfo(), nil
}

// ActivateCompanionPage is the user selecting a tab in the pane. It is
// presented first (selectTab); a suspended page is then restored, and the
// call answers when it is. A tab closed while it restored is no error.
func (m *Manager) ActivateCompanionPage(ctx context.Context, access Access, pageID string) error {
	pageID = strings.TrimSpace(pageID)
	if err := m.selectTab(access, pageID); err != nil {
		return err
	}
	return m.restoreSelectedTab(ctx, access, pageID)
}

// StepCompanionPage is the user's next/previous tab: it selects the tab step
// places from the active one in strip order, wrapping, as one change to the
// Manager's own order and selection, so presses in a row each move one tab
// whatever the client last saw. With no active tab, a forward step selects
// the first tab and a backward one the last. Then it is ActivateCompanionPage.
func (m *Manager) StepCompanionPage(ctx context.Context, access Access, step int) error {
	if step == 0 {
		return fmt.Errorf("browser: tab step must not be 0")
	}
	pageID, err := m.selectTabWhere(access, func(tabs []threadTab, active string) int {
		n := len(tabs)
		if n == 0 {
			return -1
		}
		at := -1
		for i := range tabs {
			if tabs[i].id == active {
				at = i
			}
		}
		if at < 0 && step < 0 {
			at = 0
		}
		return ((at+step)%n + n) % n
	})
	if err != nil {
		return err
	}
	return m.restoreSelectedTab(ctx, access, pageID)
}

// restoreSelectedTab restores the selected tab when it is suspended and
// answers once it is; a tab closed meanwhile is no error.
func (m *Manager) restoreSelectedTab(ctx context.Context, access Access, pageID string) error {
	m.mu.Lock()
	_, suspended := m.suspended[pageID]
	m.mu.Unlock()
	if !suspended {
		return nil
	}
	_, _, err := m.resolvePage(ctx, access, pageID, false)
	if err != nil && errors.Is(err, errPageNotFound) && !m.hasTab(access.ThreadID, pageID) {
		return nil
	}
	return err
}
