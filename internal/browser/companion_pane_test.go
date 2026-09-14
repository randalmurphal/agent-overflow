package browser

import (
	"strings"
	"sync"
	"testing"
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
}

func (e *recordingPaneHost) record(call string) {
	e.mu.Lock()
	e.calls = append(e.calls, call)
	e.mu.Unlock()
}

func (e *recordingPaneHost) ShowPage(handle string) { e.record("show:" + handle) }
func (e *recordingPaneHost) HidePage(handle string) { e.record("hide:" + handle) }
func (e *recordingPaneHost) SetPageBounds(handle string, placement PanePlacement) {
	e.record("bounds:" + handle)
	e.mu.Lock()
	onBounds := e.onBounds
	e.mu.Unlock()
	if onBounds != nil {
		onBounds(placement)
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
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	first, err := manager.Open(t.Context(), access, "https://example.test/first", OpenOptions{})
	if err != nil {
		t.Fatalf("open first page: %v", err)
	}
	second, err := manager.NewCompanionPage(t.Context(), access)
	if err != nil {
		t.Fatalf("open second page: %v", err)
	}
	engine.take()
	return manager, engine, access, first.ID, second.ID
}

func paneHandle(m *Manager, access Access, pageID string) string {
	p, _, err := m.lookupOwnedPage(access, pageID)
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
	if _, err := manager.Visibility(t.Context(), access, &show, secondID); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount, err := manager.AttachPane(access)
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	engine.take()
	rect := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	if err := manager.SetPaneRect(mount.ID, rect); err != nil {
		t.Fatalf("set pane rect: %v", err)
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
	if _, err := manager.Visibility(t.Context(), access, &show, secondID); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount, err := manager.AttachPane(access)
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
	if _, err := manager.Visibility(t.Context(), access, &show, secondID); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount, err := manager.AttachPane(access)
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	engine.take()

	var got []PanePlacement
	engine.onBounds = func(placement PanePlacement) { got = append(got, placement) }
	rect := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	if err := manager.SetPaneRect(mount.ID, rect); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no bounds reached the engine")
	}
	// The default 1280x720 page fits an 800x600 pane at 0.625: 800x450,
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
	if _, err := manager.Visibility(t.Context(), access, &show, secondID); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount, err := manager.AttachPane(access)
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	rect := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	if err := manager.SetPaneRect(mount.ID, rect); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	engine.take()

	manager.DetachPane(mount.ID)
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
