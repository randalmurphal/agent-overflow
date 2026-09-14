package browser

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The pane is a viewer over a page that keeps the thread's viewport: placePage
// scales the page DOWN to fit the host rect, never up, centers the remainder,
// and crops to the visible clip. The engines draw exactly this placement.
func TestPlacePageFitsDownCentersAndClips(t *testing.T) {
	full := func(x, y, w, h float64) PaneRect {
		return PaneRect{X: x, Y: y, Width: w, Height: h, ClipX: x, ClipY: y, ClipWidth: w, ClipHeight: h}
	}

	// A pane wider than tall against a 16:9 page: width binds, height centers.
	placement, ok := placePage(full(10, 20, 800, 600), 1280, 720)
	if !ok {
		t.Fatal("a paintable pane must place the page")
	}
	if placement.Scale != 0.625 || placement.PageWidth != 1280 || placement.PageHeight != 720 {
		t.Fatalf("scale/page = %+v", placement)
	}
	if r := placement.Rect; r.X != 10 || r.Y != 95 || r.Width != 800 || r.Height != 450 {
		t.Fatalf("fitted rect = %+v", r)
	}
	if r := placement.Rect; r.ClipX != 10 || r.ClipY != 95 || r.ClipWidth != 800 || r.ClipHeight != 450 {
		t.Fatalf("clip must shrink to the fitted rect: %+v", r)
	}

	// A pane larger than the page never scales up: scale 1, margins all round.
	placement, ok = placePage(full(0, 0, 2000, 1000), 1280, 720)
	if !ok || placement.Scale != 1 {
		t.Fatalf("a larger pane must show the page at 1:1, got %+v ok=%v", placement, ok)
	}
	if r := placement.Rect; r.X != 360 || r.Y != 140 || r.Width != 1280 || r.Height != 720 {
		t.Fatalf("page must be centered in a larger pane: %+v", r)
	}

	// Height binds on a tall, narrow page.
	placement, ok = placePage(full(0, 0, 1000, 300), 600, 900)
	if !ok || placement.Scale != 300.0/900.0 {
		t.Fatalf("height must bind: %+v ok=%v", placement, ok)
	}
	if r := placement.Rect; r.Width != 200 || r.Height != 300 || r.X != 400 || r.Y != 0 {
		t.Fatalf("tall page fitted rect = %+v", r)
	}

	// The clip is intersected with the fitted rect: a pane scrolled half under a
	// header shows the lower half of the fitted page, and one whose fitted rect
	// is entirely clipped away is not presented at all.
	rect := full(0, 0, 800, 600)
	rect.ClipY, rect.ClipHeight = 300, 300
	placement, ok = placePage(rect, 1280, 720)
	if !ok {
		t.Fatal("a half-visible page must still present")
	}
	if r := placement.Rect; r.ClipY != 300 || r.ClipHeight != 225 {
		t.Fatalf("clip must be the visible part of the fitted rect: %+v", r)
	}
	rect.ClipY, rect.ClipHeight = 0, 50
	if _, ok := placePage(rect, 1280, 720); ok {
		t.Fatal("a clip above the fitted rect leaves nothing to present")
	}

	// Nothing to fit into, or no page size, is not a placement.
	if _, ok := placePage(PaneRect{}, 1280, 720); ok {
		t.Fatal("an empty pane must not place")
	}
	if _, ok := placePage(full(0, 0, 800, 600), 0, 0); ok {
		t.Fatal("a page with no viewport must not place")
	}
}

// browser_viewport is the ONE thing that sizes a page. Every page the thread
// owns lays out at it, hidden or presented, and a reset returns them all to
// the default rather than leaving them wherever the pane happened to be.
func TestViewportLaysOutEveryOwnedPageAndResetsToTheDefault(t *testing.T) {
	manager, _, access, firstID, secondID := newPaneHostManager(t)
	first, _, err := manager.lookupOwnedPage(access, firstID)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.lookupOwnedPage(access, secondID)
	if err != nil {
		t.Fatal(err)
	}
	pages := []*fakePage{first.driver.(*fakePage), second.driver.(*fakePage)}
	for _, p := range pages {
		if w, h := p.lastViewport(); w != defaultViewportWidth || h != defaultViewportHeight {
			t.Fatalf("a new page must lay out at the default viewport, got %dx%d", w, h)
		}
	}

	info, err := manager.Viewport(t.Context(), access, ViewportOptions{Action: "set", Width: 1000, Height: 500})
	if err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	if info.ViewportW != 1000 || info.ViewportH != 500 || !info.ViewportSet {
		t.Fatalf("session info = %+v", info)
	}
	for _, p := range pages {
		if w, h := p.lastViewport(); w != 1000 || h != 500 {
			t.Fatalf("every owned page must take the viewport, got %dx%d", w, h)
		}
	}
	if w, h := sessionViewport(info); w != 1000 || h != 500 {
		t.Fatalf("sessionViewport = %dx%d", w, h)
	}

	info, err = manager.Viewport(t.Context(), access, ViewportOptions{Action: "reset"})
	if err != nil {
		t.Fatalf("reset viewport: %v", err)
	}
	if info.ViewportSet {
		t.Fatalf("reset must clear the override: %+v", info)
	}
	for _, p := range pages {
		if w, h := p.lastViewport(); w != defaultViewportWidth || h != defaultViewportHeight {
			t.Fatalf("reset must lay every page out at the default, got %dx%d", w, h)
		}
	}
}

// A page that never produces a frame answers by name within the capture's
// own bound instead of holding the page lock for the full operation timeout.
func TestScreenshotNamesAPageThatProducesNoFrame(t *testing.T) {
	previous := screenshotTimeout
	screenshotTimeout = 30 * time.Millisecond
	t.Cleanup(func() { screenshotTimeout = previous })

	manager, _, access, firstID, _ := newPaneHostManager(t)
	first, _, err := manager.lookupOwnedPage(access, firstID)
	if err != nil {
		t.Fatal(err)
	}
	fake := first.driver.(*fakePage)
	fake.mu.Lock()
	fake.screenshot = func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	fake.mu.Unlock()

	started := time.Now()
	_, err = manager.Screenshot(t.Context(), access, ScreenshotOptions{PageID: firstID})
	if err == nil || !strings.Contains(err.Error(), "produced no frame") {
		t.Fatalf("expected the no-frame error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the capture waited %s, longer than its own bound", elapsed)
	}
	// The page is still usable afterwards: the bound released its lock.
	if _, err := manager.Viewport(t.Context(), access, ViewportOptions{Action: "get"}); err != nil {
		t.Fatalf("page must stay usable after a failed capture: %v", err)
	}
}
