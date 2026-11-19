package browser

import (
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// The pane presentation contract every paneHost engine is built against
// (webkit_pane_linux.go, wkwebview_pane_darwin.go, the hosted engine): the
// Manager hides every non-presented page BEFORE presenting the active one,
// and always sends the active page's bounds BEFORE its show. An engine that
// receives a show without bounds parks the view rather than guessing, so a
// Manager that reordered these calls would present nothing — silently, and
// only on a real desktop. This test pins the ordering where it is decided.

// recordingPaneHost wraps the fake engine with a paneHost half that records
// the call sequence, standing in for a real windowed engine.
type recordingPaneHost struct {
	*fakeEngine
	mu       sync.Mutex
	calls    []string
	onBounds func(PanePlacement)
	// onBase, when set, sees every SetPageBase. Bases are not in calls.
	onBase func(handle, color string)
	// onShow, when set, sees every ShowPage after it is recorded.
	onShow func(handle string)
	// onHide, when set, sees every HidePage after it is recorded.
	onHide func(handle string)
	// focusErr answers FocusPage, and lastFocus is the request it was asked.
	focusErr  error
	lastFocus focusRequest
}

func (e *recordingPaneHost) record(call string) {
	e.mu.Lock()
	e.calls = append(e.calls, call)
	e.mu.Unlock()
}

func (e *recordingPaneHost) ShowPage(handle ids.PageHandle) {
	e.record("show:" + string(handle))
	e.mu.Lock()
	onShow := e.onShow
	e.mu.Unlock()
	if onShow != nil {
		onShow(string(handle))
	}
}
func (e *recordingPaneHost) HidePage(handle ids.PageHandle) {
	e.record("hide:" + string(handle))
	e.mu.Lock()
	onHide := e.onHide
	e.mu.Unlock()
	if onHide != nil {
		onHide(string(handle))
	}
}

// FocusPage records the request; focusErr, when set, is its answer.
func (e *recordingPaneHost) FocusPage(handle ids.PageHandle, req focusRequest) (focusResult, error) {
	e.record("focus:" + string(handle))
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastFocus = req
	if e.focusErr != nil {
		return focusNotPresented, e.focusErr
	}
	return focusMoved, nil
}

var _ paneHost = (*recordingPaneHost)(nil)

func (e *recordingPaneHost) SetPageBounds(handle ids.PageHandle, placement PanePlacement) {
	e.record("bounds:" + string(handle))
	e.mu.Lock()
	onBounds := e.onBounds
	e.mu.Unlock()
	if onBounds != nil {
		onBounds(placement)
	}
}

func (e *recordingPaneHost) SetPageBase(handle ids.PageHandle, color string) {
	e.mu.Lock()
	onBase := e.onBase
	e.mu.Unlock()
	if onBase != nil {
		onBase(string(handle), color)
	}
}

func (e *recordingPaneHost) take() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	calls := e.calls
	e.calls = nil
	return calls
}

// newPaneHostManager builds a Manager over the recording engine with two
// pages owned by one thread, and answers the engine, the access, and the two
// page ids in creation order.
func newPaneHostManager(t *testing.T) (*Manager, *recordingPaneHost, Access, string, string) {
	t.Helper()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	engine := &recordingPaneHost{fakeEngine: manager.engine.(*fakeEngine)}
	manager.engine = engine
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	first, err := manager.Open(t.Context(), access, "https://example.test/first", OpenOptions{})
	if err != nil {
		t.Fatalf("open first page: %v", err)
	}
	second, err := manager.NewCompanionPage(t.Context(), access, "")
	if err != nil {
		t.Fatalf("open second page: %v", err)
	}
	engine.take()
	return manager, engine, access, first.ID, second.ID
}

// paneHandle is the engine handle of a page as the recording pane host
// records it.
func paneHandle(m *Manager, access Access, pageID string) string {
	return string(driverHandle(m, access, pageID))
}

func driverHandle(m *Manager, access Access, pageID string) ids.PageHandle {
	p, _, err := m.lookupOwnedPage(access, PageID(pageID))
	if err != nil {
		panic(err)
	}
	return p.driver.Handle()
}

func TestPanePresentationHidesOthersThenSendsBoundsBeforeShow(t *testing.T) {
	manager, engine, access, firstID, secondID := newPaneHostManager(t)
	firstHandle := paneHandle(manager, access, firstID)
	secondHandle := paneHandle(manager, access, secondID)

	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(secondID)); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount, err := manager.AttachPane(access.Caller())
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	engine.take()
	// The rect resizes the thread viewport, so the active page is laid out at
	// it before it is shown: the presentation lands after the drain pass.
	applied := make(chan string, 8)
	manager.viewportApplied = func(threadID string) { applied <- threadID }
	rect := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	if err := manager.SetPaneRect(mount.ID, rect); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("the pane size was never applied")
	}

	calls := engine.take()
	joined := strings.Join(calls, " ")
	if !strings.Contains(joined, "hide:"+firstHandle) {
		t.Fatalf("non-active page was not hidden: %v", calls)
	}
	hideAt := strings.Index(joined, "hide:"+firstHandle)
	boundsAt := strings.Index(joined, "bounds:"+secondHandle)
	showAt := strings.Index(joined, "show:"+secondHandle)
	if boundsAt < 0 || showAt < 0 {
		t.Fatalf("active page was not presented: %v", calls)
	}
	if !(hideAt < boundsAt && boundsAt < showAt) {
		t.Fatalf("presentation order must be hide-others, bounds, show; got %v", calls)
	}
}

func TestPanePresentationStaysHiddenWithoutAPaintableRect(t *testing.T) {
	manager, engine, access, _, secondID := newPaneHostManager(t)
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(secondID)); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount, err := manager.AttachPane(access.Caller())
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	engine.take()

	// A mounted pane whose rect is off-screen (Visible false) or degenerate
	// must never produce a show: presenting at a stale or zero-sized place
	// paints a native view over whatever happens to be there.
	for _, rect := range []PaneRect{
		{X: 1, Y: 1, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: false},
		{X: 1, Y: 1, Width: 0, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true},
	} {
		if err := manager.SetPaneRect(mount.ID, rect); err != nil {
			t.Fatalf("set pane rect: %v", err)
		}
		for _, call := range engine.take() {
			if strings.HasPrefix(call, "show:") {
				t.Fatalf("rect %+v produced %s", rect, call)
			}
		}
	}
}

// A rect without clip fields means "unclipped" and must reach the engine as
// clip == rect, never as a zero clip an engine would crop everything with;
// a rect whose clip intersection is empty must never present.
func TestPaneRectClipDefaultsAndEmptyClipHides(t *testing.T) {
	manager, engine, access, _, secondID := newPaneHostManager(t)
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(secondID)); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount, err := manager.AttachPane(access.Caller())
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	engine.take()
	// A pinned viewport keeps the page larger than the pane, so the clip has
	// a fitted rect to shrink to (an unpinned page would be the pane's size).
	if _, err := manager.Viewport(t.Context(), access, ViewportOptions{Action: "set", Width: 1280, Height: 720}); err != nil {
		t.Fatalf("pin viewport: %v", err)
	}

	var got []PanePlacement
	engine.onBounds = func(placement PanePlacement) { got = append(got, placement) }
	rect := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	if err := manager.SetPaneRect(mount.ID, rect); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no bounds reached the engine")
	}
	// The pinned 1280x720 page fits an 800x600 pane at 0.625: 800x450,
	// centered 75px down, and the defaulted full-rect clip crops to that.
	last := got[len(got)-1]
	if last.PageWidth != 1280 || last.PageHeight != 720 || last.Scale != 0.625 {
		t.Fatalf("placement did not keep the thread viewport: %+v", last)
	}
	if last.Rect.X != 10 || last.Rect.Y != 95 || last.Rect.Width != 800 || last.Rect.Height != 450 {
		t.Fatalf("page was not fitted and centered in the pane: %+v", last.Rect)
	}
	if last.Rect.ClipX != 10 || last.Rect.ClipY != 95 || last.Rect.ClipWidth != 800 || last.Rect.ClipHeight != 450 {
		t.Fatalf("zero clip was not defaulted to the fitted rect: %+v", last.Rect)
	}

	engine.take()
	clipped := rect
	clipped.ClipX, clipped.ClipY, clipped.ClipWidth, clipped.ClipHeight = 10, 20, 800, 0.5
	if err := manager.SetPaneRect(mount.ID, clipped); err != nil {
		t.Fatalf("set clipped pane rect: %v", err)
	}
	for _, call := range engine.take() {
		if strings.HasPrefix(call, "show:") {
			t.Fatalf("an empty clip produced %s", call)
		}
	}
}

func TestPaneDetachHidesThePresentedView(t *testing.T) {
	manager, engine, access, _, secondID := newPaneHostManager(t)
	secondHandle := paneHandle(manager, access, secondID)
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(secondID)); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount := attachSizedPane(t, manager, engine, access)

	manager.DetachPane(mount)
	var hidden bool
	for _, call := range engine.take() {
		if call == "hide:"+secondHandle {
			hidden = true
		}
		if strings.HasPrefix(call, "show:") {
			t.Fatalf("detach produced %s", call)
		}
	}
	if !hidden {
		t.Fatal("detaching the pane left the presented view showing")
	}
}

// attachSizedPane mounts a pane, reports its rect and waits for the pages to
// take its size, whose presentation pass runs on its own goroutine; then
// nothing of the thread's presentation is in flight. It answers the mount.
func attachSizedPane(t *testing.T, manager *Manager, engine *recordingPaneHost, access Access) string {
	t.Helper()
	applied := make(chan string, 16)
	manager.viewportApplied = func(threadID string) { applied <- threadID }
	mount, err := manager.AttachPane(access.Caller())
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	rect := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	if err := manager.SetPaneRect(mount.ID, rect); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	if got := <-applied; got != access.ThreadID {
		t.Fatalf("viewport applied for %q, want %q", got, access.ThreadID)
	}
	// The drain may owe another pass (a presentation pass that saw the page
	// lag asks for one); a pass left running would race the caller's.
	awaitViewportIdle(t, manager, access.ThreadID)
	for len(applied) > 0 {
		<-applied
	}
	engine.take()
	return mount.ID
}

// awaitViewportIdle waits until the thread's viewport drain has neither a
// pass queued nor one running, and no presentation pass runs.
func awaitViewportIdle(t *testing.T, manager *Manager, threadID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.mu.Lock()
		st := manager.viewportSyncLocked(threadID)
		_, presenting := manager.presenting[threadID]
		busy := st.busy || st.dirty || presenting
		manager.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the viewport drain never went idle")
		}
		time.Sleep(time.Millisecond)
	}
}

// Presentation passes of a thread never overlap: a pass asked for while one
// is telling the engine to show the page runs after it, so a detach that
// lands mid-pass still leaves the page hidden.
func TestPanePresentationPassesNeverOverlap(t *testing.T) {
	manager, engine, access, _, secondID := newPaneHostManager(t)
	secondHandle := paneHandle(manager, access, secondID)
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(secondID)); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount := attachSizedPane(t, manager, engine, access)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	engine.mu.Lock()
	engine.onBounds = func(PanePlacement) {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	engine.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.syncPanePresentation(access.ThreadID)
	}()
	<-entered
	manager.DetachPane(mount)
	close(release)
	<-done
	last := ""
	for _, call := range engine.take() {
		if strings.HasSuffix(call, ":"+secondHandle) && !strings.HasPrefix(call, "bounds:") {
			last = call
		}
	}
	if last != "hide:"+secondHandle {
		t.Fatalf("after a detach during a presentation pass the page's last call is %q, want it hidden", last)
	}
}

// A placement whose one visible CSS pixel rounds away in the overlay's
// pixels (the SPA at 50% zoom) shows nothing in the overlay, where an
// unclipped view would cover the pane.
func TestAnOverlayPlacementWithNoPixelLeftIsHidden(t *testing.T) {
	placement := PanePlacement{
		Rect: PaneRect{
			X: 0, Y: 0, Width: 400, Height: 300,
			ClipX: 1, ClipY: 0, ClipWidth: 1, ClipHeight: 300,
			ViewportWidth: 1280, ViewportHeight: 720,
		},
		Scale: 1,
	}
	if layout, _, ok := overlayPaneLayout(placement, 640, 360); ok {
		t.Fatalf("a placement with no overlay pixel lays out as %+v", layout)
	}
}
