package browser

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/keybindings"

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
// assumes CSS pixels equal its units: it scales the rect by its own client
// size over the viewport, which makes the position exact under webview zoom
// (Ctrl+=) and any DPI without either side knowing the other's scale factor.
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
}

// PanePlacement is where a presented page's view goes: the page keeps its
// own viewport (PageWidth x PageHeight, the size the agent works against)
// and the pane shows it scaled by Scale, never resized by the placement. A
// page following the pane is its size and Scale is 1; a pinned page larger
// than the pane scales down. Rect is the FITTED rect in the SPA's CSS pixels,
// the page scaled to fit inside the pane's host rect and centered, with its
// clip already intersected with the host rect's visible clip. Rect's
// viewport, background and device pixel ratio are the host rect's own.
type PanePlacement struct {
	Rect       PaneRect
	PageWidth  int
	PageHeight int
	Scale      float64
}

// placePage fits a page of pageW x pageH CSS pixels inside a host rect: it is
// scaled down to fit (never up, so text stays sharp) and centered, and the
// fitted rect is cropped by the host's visible clip. ok is false when nothing
// of the page would be visible, which the caller treats as hidden.
func placePage(rect PaneRect, pageW, pageH int) (PanePlacement, bool) {
	if pageW <= 0 || pageH <= 0 || rect.Width < 1 || rect.Height < 1 {
		return PanePlacement{}, false
	}
	scale := math.Min(rect.Width/float64(pageW), rect.Height/float64(pageH))
	if scale > 1 {
		scale = 1
	}
	fitted := rect
	fitted.Width = float64(pageW) * scale
	fitted.Height = float64(pageH) * scale
	fitted.X = rect.X + (rect.Width-fitted.Width)/2
	fitted.Y = rect.Y + (rect.Height-fitted.Height)/2
	left := math.Max(fitted.X, rect.ClipX)
	top := math.Max(fitted.Y, rect.ClipY)
	right := math.Min(fitted.X+fitted.Width, rect.ClipX+rect.ClipWidth)
	bottom := math.Min(fitted.Y+fitted.Height, rect.ClipY+rect.ClipHeight)
	if right-left < 1 || bottom-top < 1 {
		return PanePlacement{}, false
	}
	fitted.ClipX, fitted.ClipY = left, top
	fitted.ClipWidth, fitted.ClipHeight = right-left, bottom-top
	return PanePlacement{Rect: fitted, PageWidth: pageW, PageHeight: pageH, Scale: scale}, true
}

// paneMount is one mounted pane surface: the frontend's claim that a host rect
// for this thread exists, plus the last rect it reported. The mount is the
// unit connection cleanup releases, so a dead UI can never leave a native view
// painted over a window that no longer renders the pane under it.
type paneMount struct {
	threadID string
	rect     PaneRect
	hasRect  bool
}

func (p *managedPage) setInfo(info PageInfo) {
	p.metaMu.Lock()
	info.Label = p.info.Label
	p.info = info
	p.metaMu.Unlock()
}

// setHistoryState updates only the back/forward flags and reports whether
// anything changed, so the async refresh behind an engine info event emits
// no state push when the answer is the one already shown.
func (p *managedPage) setHistoryState(canGoBack, canGoForward bool) bool {
	p.metaMu.Lock()
	changed := p.info.CanGoBack != canGoBack || p.info.CanGoForward != canGoForward
	p.info.CanGoBack, p.info.CanGoForward = canGoBack, canGoForward
	p.metaMu.Unlock()
	return changed
}

func (p *managedPage) setLabel(label string) PageInfo {
	p.metaMu.Lock()
	p.info.Label = label
	info := p.info
	p.metaMu.Unlock()
	return info
}

func (p *managedPage) cachedInfo() PageInfo {
	p.metaMu.RLock()
	info := p.info
	p.metaMu.RUnlock()
	return info
}

func (m *Manager) pageChanged(p *managedPage) {
	p.touch()
	m.ensureActivePage(p.owner, p.id)
	m.emitThreadState(p.owner)
	m.syncPanePresentation(p.owner)
}

func (m *Manager) threadState(threadID string) CompanionEvent {
	m.mu.Lock()
	pages := make([]*managedPage, 0)
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == threadID {
				pages = append(pages, p)
			}
		}
	}
	session, hasSession := m.sessions[threadID]
	m.mu.Unlock()
	sortPagesByTabOrder(pages)
	event := CompanionEvent{Kind: "state", ThreadID: threadID, Pages: make([]PageInfo, 0, len(pages))}
	visible := false
	if hasSession {
		visible = session.Visible
		event.SessionName = session.Name
	}
	event.Visible = &visible
	for _, p := range pages {
		event.Pages = append(event.Pages, p.cachedInfo())
	}
	event.ActivePageID = session.ActivePageID
	event.ViewportWidth, event.ViewportHeight = sessionViewport(session)
	event.ViewportSet = session.ViewportSet
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

func (m *Manager) emitThreadState(threadID string) {
	if strings.TrimSpace(threadID) != "" {
		m.emit(m.threadState(threadID))
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

// keyChord is engineEvents.KeyChord: it answers on the engine's UI thread, so
// it is one set lookup, and the routing that takes m.mu happens off it.
func (m *Manager) keyChord(handle string, pressed keybindings.Accelerator) bool {
	if m.accelerators == nil {
		return false
	}
	bound, ok := m.accelerators().Match(pressed)
	if !ok {
		return false
	}
	go m.emitAccelerator(handle, bound)
	return true
}

func (m *Manager) emitAccelerator(handle string, bound keybindings.Accelerator) {
	m.mu.Lock()
	p := m.pageByHandleLocked(handle)
	m.mu.Unlock()
	if p == nil {
		return
	}
	m.emit(CompanionEvent{Kind: "accelerator", ThreadID: p.owner, Accelerator: &bound})
}

// AcceleratorsChanged tells an engine that matches chords out of process
// (engineAccelerators) that the bound set it holds is stale.
func (m *Manager) AcceleratorsChanged() {
	if engine, ok := m.engine.(engineAccelerators); ok {
		engine.SyncAccelerators()
	}
}

func (m *Manager) updatePageInfo(handle, url, title string) {
	m.mu.Lock()
	found := m.pageByHandleLocked(handle)
	m.mu.Unlock()
	if found == nil {
		return
	}
	previous := found.cachedInfo()
	found.setInfo(PageInfo{
		ID:    found.id,
		URL:   truncateUTF8(url, maxBrowserURLBytes),
		Title: truncateUTF8(title, maxBrowserTitleBytes),
		// The engine event carries no history state; keep what is shown
		// and let the async refresh below correct it.
		CanGoBack: previous.CanGoBack, CanGoForward: previous.CanGoForward,
	})
	m.emitThreadState(found.owner)
	go m.refreshHistoryState(found)
}

// refreshHistoryState re-reads one page's back/forward availability after an
// engine announced a navigation, off the engine's event goroutine — the read
// is a driver round trip, and blocking the event dispatcher on it could
// deadlock an engine whose events and commands share a loop. A late answer
// only ever disables a button one push later; the next state emission wins.
func (m *Manager) refreshHistoryState(p *managedPage) {
	ctx, cancel := operationContext(context.Background(), p.driver.Lifetime(), operationTimeout)
	defer cancel()
	back, forward, err := p.driver.HistoryState(ctx)
	if err != nil {
		return
	}
	if p.setHistoryState(back, forward) {
		m.emitThreadState(p.owner)
	}
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
	m.panes[id] = paneMount{threadID: access.ThreadID}
	m.mu.Unlock()
	return CompanionSubscription{ID: id, State: state}, nil
}

// DetachPane releases one pane mount. The presentation sync runs so an engine
// with a presented native view hides it rather than painting over whatever
// replaced the pane. A mount that remains for the thread (a diagnostic mount
// released while the UI's pane stays) hands the pages back its own size.
func (m *Manager) DetachPane(id string) {
	m.mu.Lock()
	mount, ok := m.panes[id]
	resize := false
	if ok {
		delete(m.panes, id)
		for _, other := range m.panes {
			if other.threadID == mount.threadID && other.hasRect {
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
		// the drop shows it again.
		resize = m.notePaneSizeLocked(mount.threadID, rect)
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
func (m *Manager) syncPanePresentation(threadID string) {
	host, ok := m.engine.(paneHost)
	if !ok {
		return
	}
	m.mu.Lock()
	session, hasSession := m.sessions[threadID]
	shown := false
	var rect PaneRect
	for _, mount := range m.panes {
		if mount.threadID != threadID || !mount.hasRect {
			continue
		}
		if mount.rect.Visible && mount.rect.Width >= 1 && mount.rect.Height >= 1 &&
			mount.rect.ClipWidth >= 1 && mount.rect.ClipHeight >= 1 {
			shown = true
			rect = mount.rect
		}
	}
	visible := hasSession && session.Visible && shown
	var pages []*managedPage
	var active *managedPage
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
	}
	m.mu.Unlock()
	var placement PanePlacement
	if visible {
		// The page keeps the session's viewport; the pane shows it scaled to
		// fit. A pane too small or too occluded to show any of it presents
		// nothing rather than a sliver.
		pageW, pageH := sessionViewport(session)
		placement, visible = placePage(rect, pageW, pageH)
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
}

// sortPagesByTabOrder is THE tab-strip order: every surface that lists a
// thread's pages (companion state, ambiguity errors) sorts with it so the
// UI, the tools and the errors never disagree about which tab is first.
func sortPagesByTabOrder(pages []*managedPage) {
	sort.Slice(pages, func(i, j int) bool {
		oi, oj := pages[i].tabOrder.Load(), pages[j].tabOrder.Load()
		if oi != oj {
			return oi < oj
		}
		return pages[i].createdAt < pages[j].createdAt
	})
}

// MoveCompanionPage places one of the thread's pages at index in tab order
// (clamped). Order is runtime state, like the pages themselves: the moved
// prefix is renumbered 1..n, and a page opened later keeps appending at the
// end because its creation-time key is always larger.
func (m *Manager) MoveCompanionPage(access Access, pageID string, index int) error {
	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	var pages []*managedPage
	for _, scope := range m.scopes {
		for _, q := range scope.pages {
			if q.owner == access.ThreadID {
				pages = append(pages, q)
			}
		}
	}
	m.mu.Unlock()
	sortPagesByTabOrder(pages)
	ordered := make([]*managedPage, 0, len(pages))
	for _, q := range pages {
		if q != p {
			ordered = append(ordered, q)
		}
	}
	index = max(0, min(index, len(ordered)))
	ordered = append(ordered[:index], append([]*managedPage{p}, ordered[index:]...)...)
	for i, q := range ordered {
		q.tabOrder.Store(int64(i + 1))
	}
	m.emitThreadState(access.ThreadID)
	return nil
}

func (m *Manager) NewCompanionPage(ctx context.Context, access Access) (PageInfo, error) {
	p, err := m.createPage(ctx, access)
	if err != nil {
		return PageInfo{}, err
	}
	if _, err := m.SelectPage(ctx, access, p.id); err != nil {
		return PageInfo{}, err
	}
	return p.cachedInfo(), nil
}

func (m *Manager) ActivateCompanionPage(access Access, pageID string) error {
	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		return err
	}
	p.touch()
	m.setActivePage(access.ThreadID, p.id)
	m.emitThreadState(access.ThreadID)
	m.syncPanePresentation(access.ThreadID)
	return nil
}

func (m *Manager) NavigateCompanion(ctx context.Context, access Access, pageID, address string) (PageInfo, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return PageInfo{}, fmt.Errorf("browser: address is required")
	}
	lower := strings.ToLower(address)
	parsed, _ := url.Parse(address)
	if parsed != nil && strings.EqualFold(parsed.Scheme, "file") {
		localPath := filepath.FromSlash(parsed.Path)
		if engine, ok := m.engine.(engineFileURL); ok {
			// A pasted file URL is in the RENDERER's form; OpenFile wants
			// the backend path behind it.
			mapped, err := engine.BackendFilePath(ctx, address)
			if err != nil {
				return PageInfo{}, err
			}
			localPath = mapped
		}
		return m.OpenFile(ctx, access, localPath, OpenOptions{PageID: pageID})
	}
	if filepath.IsAbs(address) {
		return m.OpenFile(ctx, access, address, OpenOptions{PageID: pageID})
	}
	workspaceFile := filepath.Join(access.Workspace, filepath.FromSlash(address))
	if info, err := os.Stat(workspaceFile); err == nil && info.Mode().IsRegular() {
		return m.OpenFile(ctx, access, workspaceFile, OpenOptions{PageID: pageID})
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		host := address
		if before, _, ok := strings.Cut(address, ":"); ok {
			host = before
		}
		isHost := strings.Contains(host, ".") || strings.EqualFold(host, "localhost") || net.ParseIP(strings.Trim(host, "[]")) != nil
		if strings.ContainsAny(address, " \t\r\n") || !isHost {
			address = "https://www.google.com/search?q=" + url.QueryEscape(address)
		} else if strings.EqualFold(host, "localhost") || net.ParseIP(strings.Trim(host, "[]")) != nil {
			address = "http://" + address
		} else {
			address = "https://" + address
		}
	}
	return m.Open(ctx, access, address, OpenOptions{PageID: pageID})
}
