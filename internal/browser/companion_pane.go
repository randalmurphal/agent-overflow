package browser

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/browser/turnorder"
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
// Background is the pane surface's resolved CSS color ("#rrggbb"): the
// presenting pane's is the base of its thread's blank pages (pageBase).
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
// visible clip. Rect's viewport and device pixel ratio are the host rect's
// own, and it carries no color: a page's base is set apart from where it
// sits (applyPageBase). Mount is the pane mount whose rect it was placed in, which
// lets a host that measures the mount itself place the page from its own,
// newer rect (the Windows launcher).
type PanePlacement struct {
	Rect       PaneRect
	PageWidth  int
	PageHeight int
	Scale      float64
	Mount      MountID
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
	fitted.Background = ""
	return PanePlacement{Rect: fitted, PageWidth: pageW, PageHeight: pageH, Scale: scale}, true
}

// overlayPaneLayout resolves a placement for a host that positions in whole
// pixels of an overlay covering the SPA window (the WebKitGTK pane overlay):
// the CSS-to-overlay proportion is the overlay's size over the SPA viewport,
// so it carries DPI and webview zoom without either side knowing them; no
// viewport means the rect is already in overlay pixels. The edges snap with
// paneplace.Layout. The answered scale draws the page's CSS pixels at the
// placement's scale in overlay pixels; ok is false when no overlay pixel of
// the page is visible, and the page is hidden then.
func overlayPaneLayout(placement PanePlacement, overlayW, overlayH int) (layout paneplace.HostLayout, overlayScale float64, ok bool) {
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
	layout, ok = paneplace.Layout(paneplace.Rect{
		X: rect.X, Y: rect.Y, W: rect.Width, H: rect.Height,
		ClipX: rect.ClipX, ClipY: rect.ClipY, ClipW: rect.ClipWidth, ClipH: rect.ClipHeight,
	}, sx, sy)
	return layout, scale * sx, ok
}

// paneMount is one mounted pane surface: the frontend's claim that a host rect
// for this thread exists, plus the last rect it reported. The mount is the
// unit connection cleanup releases, so a dead UI can never leave a native view
// painted over a window that no longer renders the pane under it.
type paneMount struct {
	// owner is the thread lifetime the mount attached to: it presents that
	// lifetime's pages and orders that lifetime's commands, and nothing
	// after the lifetime's close (CloseThread ends its order).
	owner   *threadBrowser
	rect    PaneRect
	hasRect bool
	// seq is the mount's place in attach order, which decides between two
	// mounts that could both present the thread's page (presenterLocked).
	seq uint64
	// findSeq is the sequence of the newest find start the Manager took
	// from this mount (admitFindStart).
	findSeq uint64
	// focus is the mount's page focus intent (focus_intent.go).
	focus *mountFocus
	// order admits the numbered tab commands made in the mount in the
	// user's order (selection.go).
	order *turnorder.Order
}

// mountLocked answers the pane mount id when it is attached to the thread
// lifetime tb and tb is still its thread's: the one admission of a mount
// for every entry point that takes one (a find, a focus intent, a numbered
// command), so a mount of a lifetime the thread's close retired reaches
// nothing of a later one. The caller holds m.mu.
func (m *Manager) mountLocked(tb *threadBrowser, id MountID) (paneMount, bool) {
	mount, ok := m.panes[id]
	if !ok || tb == nil || mount.owner != tb || tb.detached.Load() {
		return paneMount{}, false
	}
	return mount, true
}

// AttachPane registers one mounted pane surface for a thread and answers the
// state snapshot the pane renders its chrome from. The returned id is what
// SetPaneRect addresses and what DetachPane (or connection cleanup) releases.
//
// The mount belongs to the thread's lifetime current at the attach.
func (m *Manager) AttachPane(caller Caller) (CompanionSubscription, error) {
	state := m.threadState(caller.ThreadID)
	if len(state.Pages) == 0 {
		return CompanionSubscription{}, fmt.Errorf("browser: thread has no open pages")
	}
	id := MountID(uuid.NewString())
	m.mu.Lock()
	tb := m.currentLocked(caller.ThreadID)
	if tb == nil {
		m.mu.Unlock()
		return CompanionSubscription{}, fmt.Errorf("browser: thread has no open pages")
	}
	if len(m.panes) >= maxCompanionPanes {
		m.mu.Unlock()
		return CompanionSubscription{}, fmt.Errorf("browser: too many mounted browser panes")
	}
	m.paneSeq++
	m.panes[id] = paneMount{owner: tb, seq: m.paneSeq}
	m.mu.Unlock()
	return CompanionSubscription{ID: string(id), State: state}, nil
}

// DetachPane releases one pane mount. The presentation sync runs so an engine
// with a presented native view hides it rather than painting over whatever
// replaced the pane. A mount that remains for the thread becomes the
// presenter if this one was it (presenterLocked) and hands the pages its
// own size and color.
func (m *Manager) DetachPane(id MountID) {
	m.mu.Lock()
	mount, ok := m.panes[id]
	resize := false
	var recolor []*managedPage
	if ok {
		delete(m.panes, id)
		// The mount's waiting commands are passed.
		if mount.order != nil {
			mount.order.End()
		}
		// Unmounting is a focus choice: a request in flight must not land.
		if mount.focus != nil && mount.focus.intent != nil {
			mount.focus.intent.withdrawn.Store(true)
		}
		if next, _, _ := m.presenterLocked(mount.owner); next != "" {
			if other := m.panes[next]; other.hasRect {
				resize = m.notePaneSizeLocked(other.owner, other.rect)
				recolor = m.notePaneColorLocked(other.owner, other.rect)
			}
		}
	}
	m.mu.Unlock()
	for _, p := range recolor {
		m.applyPageBase(p)
	}
	if ok {
		if resize {
			m.scheduleViewport(mount.owner.id)
		}
		m.syncPanePresentation(mount.owner.id)
	}
}

// notePaneSizeLocked records a mounted pane's size on its thread lifetime's
// session and reports whether the pages should take it: the size changed
// and no viewport is pinned. Caller holds m.mu.
func (m *Manager) notePaneSizeLocked(tb *threadBrowser, rect PaneRect) bool {
	w, h, sized := paneViewportSize(rect)
	if !sized {
		return false
	}
	info := &tb.session
	if info.PaneW == w && info.PaneH == h {
		return false
	}
	info.PaneW, info.PaneH = w, h
	info.resolveViewport()
	info.UpdatedAt = time.Now()
	return !info.ViewportSet
}

// SetPaneRect records the mounted pane's current host rect. The frontend
// coalesces to one report per changed frame, so this path must stay cheap: a
// bookkeeping write, one presentation sync, and while no viewport is pinned a
// latest-wins request for the pages to take the rect's size (viewport.go).
func (m *Manager) SetPaneRect(id MountID, rect PaneRect) error {
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
	mount, ok := m.panes[id.trim()]
	resize := false
	var recolor []*managedPage
	if ok {
		mount.rect = rect
		mount.hasRect = true
		m.panes[id.trim()] = mount
		// The size is tracked whether or not the rect is paintable: a drag
		// hides the view and the page must already be at the new size when
		// the drop shows it again. Only the presenter's rect sizes the pages
		// and colors their blank bases.
		if presenter, _, _ := m.presenterLocked(mount.owner); presenter == id.trim() {
			resize = m.notePaneSizeLocked(mount.owner, rect)
			recolor = m.notePaneColorLocked(mount.owner, rect)
		}
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("browser: pane mount not found")
	}
	for _, p := range recolor {
		m.applyPageBase(p)
	}
	if resize {
		m.scheduleViewport(mount.owner.id)
	}
	m.syncPanePresentation(mount.owner.id)
	return nil
}

// paneCanShow reports whether a mount's rect can show a page: reported,
// visible, and neither empty nor clipped away.
func paneCanShow(mount paneMount) bool {
	return mount.hasRect && mount.rect.Visible && mount.rect.Width >= 1 && mount.rect.Height >= 1 &&
		mount.rect.ClipWidth >= 1 && mount.rect.ClipHeight >= 1
}

// presenterLocked answers the one mount the thread lifetime tb's pages are
// presented in, its rect, and whether that rect can show a page now. A native view is
// placed at exactly one rect, so every reader asks this one rule and none
// chooses for itself: the presentation, the pages' size, a routed chord and
// a routed Escape. The presenter is the lifetime's earliest attached live
// mount. Attach order only grows, so the role stays with a mount until it
// detaches, through a hidden or off-screen spell (a drag, a compact screen),
// and a mount attached later never takes it. Only a mount the host window
// attached exists here (BrowserCompanionPaneAttach). The caller holds m.mu.
func (m *Manager) presenterLocked(tb *threadBrowser) (MountID, PaneRect, bool) {
	var id MountID
	var mount paneMount
	if tb == nil {
		return "", PaneRect{}, false
	}
	for candidate, other := range m.panes {
		if other.owner == tb && (id == "" || other.seq < mount.seq) {
			id, mount = candidate, other
		}
	}
	if id == "" {
		return "", PaneRect{}, false
	}
	return id, mount.rect, paneCanShow(mount)
}

// presentedPageLocked answers the page a mounted pane shows: tb's active
// page while its companion is visible and a pane can show it, or "". The
// caller holds m.mu.
func (m *Manager) presentedPageLocked(tb *threadBrowser) PageID {
	if tb == nil || !tb.session.Visible {
		return ""
	}
	session := tb.session
	_, rect, shown := m.presenterLocked(tb)
	if !shown {
		return ""
	}
	pageW, pageH := sessionViewport(session)
	if _, ok := placePage(rect, pageW, pageH); !ok {
		return ""
	}
	return session.activePage()
}

// OpenPaneDevTools opens the engine's inspector for one of the thread's pages.
// Only engines with an inspector they can open implement paneDevTools;
// WKWebView (Safari's Develop menu is the inspector) and the fake engine
// answer with an explained refusal rather than a silent no-op.
func (m *Manager) OpenPaneDevTools(ctx context.Context, access Access, pageID PageID) error {
	if err := access.bound(); err != nil {
		return err
	}
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
