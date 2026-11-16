package browser

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// Every page a thread owns lays out at one size, the thread viewport, hidden
// or presented. By default it follows the mounted pane's host rect, so a
// presented page fills the pane at 1:1 and reflows when the pane resizes, the
// way a tab does in any browser. browser_viewport set pins a size instead and
// the pane draws that page scaled down to fit. Without a mounted pane the last
// pane size stands; a thread that never mounted one lays out at the default.

// sessionViewport is the size every page of the thread lays out at.
func sessionViewport(session SessionInfo) (int, int) {
	if session.ViewportW > 0 && session.ViewportH > 0 {
		return session.ViewportW, session.ViewportH
	}
	return defaultViewportWidth, defaultViewportHeight
}

// resolveViewport derives ViewportW/H for a session with no pinned size: the
// pane size when one is known, the default otherwise. A pinned session keeps
// the size the agent asked for.
func (s *SessionInfo) resolveViewport() {
	if s.ViewportSet {
		return
	}
	if s.PaneW > 0 && s.PaneH > 0 {
		s.ViewportW, s.ViewportH = s.PaneW, s.PaneH
		return
	}
	s.ViewportW, s.ViewportH = defaultViewportWidth, defaultViewportHeight
}

// paneViewportSize is the page size a host rect calls for: whole CSS pixels no
// larger than the rect, so a page at that size shows at 1:1 with at most a
// subpixel margin, held inside the viewport bounds. A rect below the minimum
// lays the page out at the minimum and the pane scales it down. ok is false
// for a rect with nothing to size against.
func paneViewportSize(rect PaneRect) (int, int, bool) {
	if rect.Width < 1 || rect.Height < 1 {
		return 0, 0, false
	}
	w := min(max(int(math.Floor(rect.Width)), minCompanionWidth), maxCompanionWidth)
	h := min(max(int(math.Floor(rect.Height)), minCompanionHeight), maxCompanionHeight)
	return w, h, true
}

// Viewport is the browser_viewport tool: get reports the size the thread's
// pages lay out at, whether it is pinned, and the pane size it would follow;
// set pins a bounded size; reset returns to following the pane.
func (m *Manager) Viewport(_ context.Context, access Access, opts ViewportOptions) (SessionInfo, error) {
	m.mu.Lock()
	info := m.sessionLocked(access.ThreadID)
	switch strings.ToLower(strings.TrimSpace(opts.Action)) {
	case "get", "":
		m.mu.Unlock()
		return info, nil
	case "reset":
		info.ViewportSet = false
		info.resolveViewport()
	case "set":
		if opts.Width < minCompanionWidth || opts.Width > maxCompanionWidth || opts.Height < minCompanionHeight || opts.Height > maxCompanionHeight {
			m.mu.Unlock()
			return SessionInfo{}, fmt.Errorf("browser: viewport must be between %dx%d and %dx%d", minCompanionWidth, minCompanionHeight, maxCompanionWidth, maxCompanionHeight)
		}
		info.ViewportW, info.ViewportH = opts.Width, opts.Height
		info.ViewportSet = true
	default:
		m.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("browser: viewport action must be get, set, or reset")
	}
	info.UpdatedAt = time.Now()
	m.sessions[access.ThreadID] = info
	m.mu.Unlock()
	if err := m.applyThreadViewport(access.ThreadID); err != nil {
		return SessionInfo{}, err
	}
	m.emitThreadState(access.ThreadID)
	m.syncPanePresentation(access.ThreadID)
	return info, nil
}

// viewportSync serializes laying a thread's pages out at its viewport. A pane
// drag reports a rect per frame and each layout is an engine round trip, so
// pane sizes are applied latest-wins by one drain goroutine per thread
// (busy/dirty, guarded by Manager.mu), and a browser_viewport call applies
// inline under the same apply lock so the two can never leave pages at each
// other's size.
//
// A pane resize lays out only the thread's active page, the one the pane
// shows. Every other page lags until it is needed: resolvePage brings a page
// to the viewport before any operation on it (coordinates and screenshots
// mean the current viewport), and presentPanes never newly shows a page that
// lags; it asks the drain to lay the page out first.
type viewportSync struct {
	apply sync.Mutex
	busy  bool
	dirty bool
}

// viewportSyncLocked answers the thread's sync record. Caller holds m.mu.
func (m *Manager) viewportSyncLocked(threadID string) *viewportSync {
	st := m.viewportSyncs[threadID]
	if st == nil {
		st = &viewportSync{}
		m.viewportSyncs[threadID] = st
	}
	return st
}

// scheduleViewport asks for the thread's active page to take the session
// viewport and returns at once; the newest request while a pass runs is the
// only one applied after it.
func (m *Manager) scheduleViewport(threadID string) {
	m.mu.Lock()
	st := m.viewportSyncLocked(threadID)
	st.dirty = true
	if st.busy {
		m.mu.Unlock()
		return
	}
	st.busy = true
	m.mu.Unlock()
	go m.drainViewport(threadID, st)
}

// drainViewport runs passes until no request is left. Each pass lays the
// active page out and re-presents the thread; the thread's state, which
// carries the viewport, is pushed once when the drain settles rather than
// once per pass.
func (m *Manager) drainViewport(threadID string, st *viewportSync) {
	for {
		m.mu.Lock()
		if !st.dirty {
			st.busy = false
			m.mu.Unlock()
			m.emitThreadState(threadID)
			return
		}
		st.dirty = false
		m.mu.Unlock()
		// A page left at the old size while the pane places it at the new
		// one is visibly wrong, so the failure replaces the pane body.
		if err := m.applyActiveViewport(threadID, st); err != nil {
			m.emit(CompanionEvent{Kind: "error", ThreadID: threadID, Error: err.Error()})
		}
		m.syncPanePresentation(threadID)
		if m.viewportApplied != nil {
			m.viewportApplied(threadID)
		}
	}
}

// applyActiveViewport lays the thread's active page out at the session
// viewport. A page already at it costs nothing.
func (m *Manager) applyActiveViewport(threadID string, st *viewportSync) error {
	m.mu.Lock()
	active, _ := m.livePageLocked(m.sessionLocked(threadID).ActivePageID)
	m.mu.Unlock()
	if active == nil || active.owner != threadID {
		return nil
	}
	st.apply.Lock()
	defer st.apply.Unlock()
	if err := m.syncPageViewport(active); err != nil && active.ctx.Err() == nil {
		return fmt.Errorf("%w (page %s)", err, active.id)
	}
	return nil
}

// applyThreadViewport lays every page the thread owns out at the session
// viewport as it stands when each page is reached. Every page is attempted;
// the first failure of a page that is not closing is the answer.
func (m *Manager) applyThreadViewport(threadID string) error {
	m.mu.Lock()
	st := m.viewportSyncLocked(threadID)
	m.mu.Unlock()
	st.apply.Lock()
	defer st.apply.Unlock()
	var firstErr error
	for _, p := range m.ownedPages(threadID) {
		p.mu.Lock()
		err := m.applyViewportLocked(p)
		p.mu.Unlock()
		if err != nil && p.ctx.Err() == nil && firstErr == nil {
			firstErr = fmt.Errorf("%w (page %s)", err, p.id)
		}
	}
	return firstErr
}

// syncPageViewport lays a page out at its thread's viewport unless it is
// already there: the lazy half of a pane resize, run before a page is shown
// or operated on.
func (m *Manager) syncPageViewport(p *managedPage) error {
	if m.pageViewportCurrent(p) {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.pageViewportCurrent(p) {
		return nil
	}
	return m.applyViewportLocked(p)
}

// pageViewportCurrent reports whether the page lays out at its thread's
// viewport now.
func (m *Manager) pageViewportCurrent(p *managedPage) bool {
	m.mu.Lock()
	width, height := sessionViewport(m.sessionLocked(p.owner))
	m.mu.Unlock()
	return p.viewport.Load() == packViewport(width, height)
}

// packViewport is a page size as one comparable word; 0 is none.
func packViewport(width, height int) uint64 {
	return uint64(uint32(width))<<32 | uint64(uint32(height))
}

// applyViewport pins a page to its thread's viewport. Every page gets one at
// creation, so a hidden page lays out and captures at a real size instead of
// whatever its parked view happens to measure, and the size is the same on
// every engine.
func (m *Manager) applyViewport(p *managedPage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return m.applyViewportLocked(p)
}

// applyViewportLocked is applyViewport with p.mu held by the caller. It
// records the size the page took, or the size it refused.
func (m *Manager) applyViewportLocked(p *managedPage) error {
	m.mu.Lock()
	width, height := sessionViewport(m.sessionLocked(p.owner))
	m.mu.Unlock()
	ctx, cancel := operationContext(context.Background(), p.ctx, 5*time.Second)
	defer cancel()
	if err := p.driver.SetViewport(ctx, width, height); err != nil {
		p.viewportFailed.Store(packViewport(width, height))
		return fmt.Errorf("browser: apply viewport: %w", err)
	}
	p.viewport.Store(packViewport(width, height))
	p.viewportFailed.Store(0)
	return nil
}
