package browser

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The thread viewport follows the pane by default: a page fills the pane at
// 1:1 and reflows as it resizes, like a tab in any browser. browser_viewport
// set is the explicit act of pinning a size, and reset returns to the pane.
// These tests pin that contract at the Manager, over the fake engine and the
// recording pane host.

type paneFollowFixture struct {
	manager *Manager
	engine  *recordingPaneHost
	access  Access
	pages   []*fakePage
	// applied receives the thread id after each asynchronous pass that laid
	// the pages out at the pane size (the Manager's viewportApplied seam).
	applied chan string
	// events collects every companion event the Manager emits.
	events chan CompanionEvent
	// placements collects every SetPageBounds the engine received, under
	// its own lock because the drain goroutine sends them.
	mu         sync.Mutex
	placements []PanePlacement
}

func newPaneFollowFixture(t *testing.T) *paneFollowFixture {
	t.Helper()
	manager, engine, access, firstID, secondID := newPaneHostManager(t)
	f := &paneFollowFixture{
		manager: manager,
		engine:  engine,
		access:  access,
		applied: make(chan string, 64),
		events:  make(chan CompanionEvent, 256),
	}
	for _, id := range []string{firstID, secondID} {
		p, _, err := manager.lookupOwnedPage(access, id)
		if err != nil {
			t.Fatal(err)
		}
		f.pages = append(f.pages, p.driver.(*fakePage))
	}
	manager.viewportApplied = func(threadID string) { f.applied <- threadID }
	manager.SetEventSink(func(event CompanionEvent) {
		select {
		case f.events <- event:
		default:
		}
	})
	engine.onBounds = func(placement PanePlacement) {
		f.mu.Lock()
		f.placements = append(f.placements, placement)
		f.mu.Unlock()
	}
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, secondID); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	return f
}

func (f *paneFollowFixture) attach(t *testing.T) string {
	t.Helper()
	mount, err := f.manager.AttachPane(f.access)
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	return mount.ID
}

// awaitPagesAt waits for a drain pass to have laid every page out at w x h,
// through the seam rather than wall-clock polling.
func (f *paneFollowFixture) awaitPagesAt(t *testing.T, w, h int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if f.pagesAt(w, h) {
			return
		}
		select {
		case <-f.applied:
		case <-deadline:
			pw, ph := f.pages[0].lastViewport()
			t.Fatalf("pages never reached %dx%d; first page is at %dx%d", w, h, pw, ph)
		}
	}
}

func (f *paneFollowFixture) pagesAt(w, h int) bool {
	for _, p := range f.pages {
		if pw, ph := p.lastViewport(); pw != w || ph != h {
			return false
		}
	}
	return true
}

func (f *paneFollowFixture) lastPlacement(t *testing.T) PanePlacement {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.placements) == 0 {
		t.Fatal("no bounds reached the engine")
	}
	return f.placements[len(f.placements)-1]
}

func (f *paneFollowFixture) session(t *testing.T) SessionInfo {
	t.Helper()
	info, err := f.manager.Viewport(t.Context(), f.access, ViewportOptions{Action: "get"})
	if err != nil {
		t.Fatalf("viewport get: %v", err)
	}
	return info
}

func paneRect(w, h float64) PaneRect {
	return PaneRect{X: 10, Y: 20, Width: w, Height: h, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
}

func TestPagesFollowThePaneUntilAViewportIsPinned(t *testing.T) {
	f := newPaneFollowFixture(t)
	mount := f.attach(t)

	// A fractional host rect lays the pages out at its whole-pixel floor and
	// the pane shows them at 1:1: no scaling, page size equals the pane's.
	if err := f.manager.SetPaneRect(mount, paneRect(800.7, 600.2)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	f.awaitPagesAt(t, 800, 600)
	info := f.session(t)
	if info.ViewportSet || info.ViewportW != 800 || info.ViewportH != 600 || info.PaneW != 800 || info.PaneH != 600 {
		t.Fatalf("a following session must report the pane size unpinned: %+v", info)
	}
	if placement := f.lastPlacement(t); placement.PageWidth != 800 || placement.PageHeight != 600 || placement.Scale != 1 {
		t.Fatalf("a following page must be placed at its own size at 1:1: %+v", placement)
	}
	awaitStateEvent(t, f.events, func(event CompanionEvent) bool {
		return event.ViewportWidth == 800 && event.ViewportHeight == 600 && !event.ViewportSet
	})

	// Pinning is explicit: the pages take the pinned size at once, the pane
	// scales it down, and later pane sizes are recorded but not followed.
	info, err := f.manager.Viewport(t.Context(), f.access, ViewportOptions{Action: "set", Width: 1000, Height: 500})
	if err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	if !info.ViewportSet || info.ViewportW != 1000 || info.ViewportH != 500 || !f.pagesAt(1000, 500) {
		t.Fatalf("set must pin every page synchronously: %+v", info)
	}
	if placement := f.lastPlacement(t); placement.PageWidth != 1000 || placement.Scale >= 1 {
		t.Fatalf("a pinned page wider than the pane must scale down: %+v", placement)
	}
	awaitStateEvent(t, f.events, func(event CompanionEvent) bool { return event.ViewportSet && event.ViewportWidth == 1000 })
	if err := f.manager.SetPaneRect(mount, paneRect(900, 700)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	info = f.session(t)
	if !info.ViewportSet || info.ViewportW != 1000 || info.ViewportH != 500 || info.PaneW != 900 || info.PaneH != 700 {
		t.Fatalf("a pinned session keeps its size and records the pane: %+v", info)
	}
	if !f.pagesAt(1000, 500) {
		t.Fatal("a pane resize must not relayout pinned pages")
	}
	select {
	case <-f.applied:
		t.Fatal("a pane resize under a pinned viewport must schedule no follow pass")
	default:
	}

	// Reset returns to the pane at the size it has now, synchronously.
	info, err = f.manager.Viewport(t.Context(), f.access, ViewportOptions{Action: "reset"})
	if err != nil {
		t.Fatalf("reset viewport: %v", err)
	}
	if info.ViewportSet || info.ViewportW != 900 || info.ViewportH != 700 || !f.pagesAt(900, 700) {
		t.Fatalf("reset must lay every page out at the pane size: %+v", info)
	}
	if placement := f.lastPlacement(t); placement.PageWidth != 900 || placement.Scale != 1 {
		t.Fatalf("a reset page must be placed at the pane size: %+v", placement)
	}

	// Detaching the pane keeps the last size: hidden pages stay where the
	// user last saw them, so screenshots with the pane closed match.
	f.manager.DetachPane(mount)
	info = f.session(t)
	if info.ViewportW != 900 || info.ViewportH != 700 || info.PaneW != 900 || info.PaneH != 700 {
		t.Fatalf("detach must keep the last pane size: %+v", info)
	}
}

func TestPaneBelowTheMinimumLaysOutAtTheMinimumAndScalesDown(t *testing.T) {
	f := newPaneFollowFixture(t)
	mount := f.attach(t)
	if err := f.manager.SetPaneRect(mount, paneRect(300, 200)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	f.awaitPagesAt(t, minCompanionWidth, minCompanionHeight)
	placement := f.lastPlacement(t)
	if placement.PageWidth != minCompanionWidth || placement.PageHeight != minCompanionHeight {
		t.Fatalf("placement page size = %dx%d", placement.PageWidth, placement.PageHeight)
	}
	if want := 200.0 / float64(minCompanionHeight); placement.Scale != want {
		t.Fatalf("placement scale = %v, want %v", placement.Scale, want)
	}
	if info := f.session(t); info.PaneW != minCompanionWidth || info.PaneH != minCompanionHeight {
		t.Fatalf("the recorded pane size must be the clamped one: %+v", info)
	}
}

// A drag reports a rect per frame; the pages end at the last one without a
// pass per report queueing behind the others.
func TestPaneResizeAppliesTheLatestSize(t *testing.T) {
	f := newPaneFollowFixture(t)
	mount := f.attach(t)
	for _, size := range [][2]float64{{700, 500}, {640, 480}, {1000, 800}} {
		if err := f.manager.SetPaneRect(mount, paneRect(size[0], size[1])); err != nil {
			t.Fatalf("set pane rect: %v", err)
		}
	}
	f.awaitPagesAt(t, 1000, 800)
	if info := f.session(t); info.ViewportW != 1000 || info.ViewportH != 800 {
		t.Fatalf("session = %+v", info)
	}
}

// A page that refuses the pane size is left at the old one while the pane
// places it at the new one, which is visibly wrong: the failure names the
// page and reaches the pane as an error event instead of vanishing in a
// goroutine.
func TestPaneSizeFailureReachesThePane(t *testing.T) {
	f := newPaneFollowFixture(t)
	f.pages[0].mu.Lock()
	f.pages[0].viewportErr = errors.New("engine refused")
	f.pages[0].mu.Unlock()
	mount := f.attach(t)
	if err := f.manager.SetPaneRect(mount, paneRect(800, 600)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-f.events:
			if event.Kind != "error" {
				continue
			}
			if !strings.Contains(event.Error, "engine refused") || !strings.Contains(event.Error, "page ") {
				t.Fatalf("error event must name the failure and the page: %q", event.Error)
			}
			return
		case <-deadline:
			t.Fatal("no error event reached the pane")
		}
	}
}

// The same failure on an explicit browser_viewport call is the call's error.
func TestViewportSetReportsAPageThatRefusesTheSize(t *testing.T) {
	f := newPaneFollowFixture(t)
	f.pages[1].mu.Lock()
	f.pages[1].viewportErr = errors.New("engine refused")
	f.pages[1].mu.Unlock()
	_, err := f.manager.Viewport(t.Context(), f.access, ViewportOptions{Action: "set", Width: 1000, Height: 500})
	if err == nil || !strings.Contains(err.Error(), "engine refused") {
		t.Fatalf("set must surface the refusing page, got %v", err)
	}
	// Every page was still attempted: the healthy one took the size.
	if w, h := f.pages[0].lastViewport(); w != 1000 || h != 500 {
		t.Fatalf("the healthy page must still take the size, got %dx%d", w, h)
	}
}

// A second mount for the thread (the harness screenshot path mounts one
// beside the UI's pane) takes the pages to its size; releasing it hands them
// back to the mount that stays instead of leaving them at a size no pane has.
func TestDetachingAMountHandsPagesBackToTheRemainingPane(t *testing.T) {
	f := newPaneFollowFixture(t)
	first := f.attach(t)
	if err := f.manager.SetPaneRect(first, paneRect(800, 600)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	f.awaitPagesAt(t, 800, 600)

	second := f.attach(t)
	if err := f.manager.SetPaneRect(second, paneRect(900, 700)); err != nil {
		t.Fatalf("set second pane rect: %v", err)
	}
	f.awaitPagesAt(t, 900, 700)

	f.manager.DetachPane(second)
	f.awaitPagesAt(t, 800, 600)
	if info := f.session(t); info.PaneW != 800 || info.PaneH != 600 || info.ViewportW != 800 {
		t.Fatalf("detach must restore the remaining pane's size: %+v", info)
	}
}

func TestPaneViewportSizeFloorsAndClamps(t *testing.T) {
	cases := []struct {
		w, h   float64
		wantW  int
		wantH  int
		wantOK bool
	}{
		{800.9, 600.1, 800, 600, true},
		{100, 100, minCompanionWidth, minCompanionHeight, true},
		{5000, 5000, maxCompanionWidth, maxCompanionHeight, true},
		{0, 600, 0, 0, false},
		{800, 0.5, 0, 0, false},
	}
	for _, c := range cases {
		w, h, ok := paneViewportSize(PaneRect{Width: c.w, Height: c.h})
		if w != c.wantW || h != c.wantH || ok != c.wantOK {
			t.Fatalf("paneViewportSize(%vx%v) = %d,%d,%v; want %d,%d,%v", c.w, c.h, w, h, ok, c.wantW, c.wantH, c.wantOK)
		}
	}
}

func awaitStateEvent(t *testing.T, events chan CompanionEvent, match func(CompanionEvent) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == "state" && match(event) {
				return
			}
		case <-deadline:
			t.Fatal("the expected state event never arrived")
		}
	}
}
