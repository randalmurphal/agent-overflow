package browser

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
type PaneRect struct {
	X              float64 `json:"x"`
	Y              float64 `json:"y"`
	Width          float64 `json:"width"`
	Height         float64 `json:"height"`
	ClipX          float64 `json:"clipX"`
	ClipY          float64 `json:"clipY"`
	ClipWidth      float64 `json:"clipWidth"`
	ClipHeight     float64 `json:"clipHeight"`
	ViewportWidth  float64 `json:"viewportWidth"`
	ViewportHeight float64 `json:"viewportHeight"`
	Visible        bool    `json:"visible"`
	Background     string  `json:"background,omitempty"`
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

func (m *Manager) updatePageInfo(handle, url, title string) {
	m.mu.Lock()
	var found *managedPage
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.driver.Handle() == handle {
				found = p
				break
			}
		}
		if found != nil {
			break
		}
	}
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
// replaced the pane.
func (m *Manager) DetachPane(id string) {
	m.mu.Lock()
	mount, ok := m.panes[id]
	if ok {
		delete(m.panes, id)
	}
	m.mu.Unlock()
	if ok {
		m.syncPanePresentation(mount.threadID)
	}
}

// SetPaneRect records the mounted pane's current host rect. The frontend
// coalesces to one report per changed frame, so this path must stay cheap: a
// bookkeeping write and one presentation sync.
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
	if ok {
		mount.rect = rect
		mount.hasRect = true
		m.panes[strings.TrimSpace(id)] = mount
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("browser: pane mount not found")
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
	for _, p := range pages {
		if p == active && visible {
			continue
		}
		host.HidePage(p.driver.Handle())
	}
	if visible && active != nil {
		host.SetPageBounds(active.driver.Handle(), rect)
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
