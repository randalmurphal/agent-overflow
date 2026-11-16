package browser

import (
	"context"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

// hostedScaleRecorder counts the scales a hosted page is asked for, through
// the engine's seam.
type hostedScaleRecorder struct {
	mu    sync.Mutex
	asked []viewScale
}

func (r *hostedScaleRecorder) record(_ string, want viewScale) {
	r.mu.Lock()
	r.asked = append(r.asked, want)
	r.mu.Unlock()
}

func (r *hostedScaleRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked)
}

func newHostedGeometryEngine(t *testing.T) (*hostedEngine, *directiveSink, *hostedScaleRecorder) {
	t.Helper()
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	engine.bind("page1", "TARGET-1")
	recorder := &hostedScaleRecorder{}
	engine.mu.Lock()
	engine.drivers["page1"] = &cdpPage{ctx: t.Context()}
	engine.viewScaleAsked = recorder.record
	engine.readPageRatio = func(context.Context, *cdpPage) (float64, error) { return 1.25, nil }
	engine.mu.Unlock()
	return engine, sink, recorder
}

// A pane that only moves (the strip scrolling, a neighbor's divider) sends
// the launcher its new place and asks the page for nothing: no device
// metrics override, no pixel-ratio read.
func TestHostedPositionOnlyChangeDoesNoMetricsWork(t *testing.T) {
	engine, sink, recorder := newHostedGeometryEngine(t)
	pane := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ClipX: 10, ClipY: 20, ClipWidth: 800, ClipHeight: 600,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 1.25, Visible: true}
	placement, _ := placePage(pane, 800, 600)
	engine.SetPageBounds("page1", placement)
	engine.ShowPage("page1")
	asked := recorder.count()
	if asked == 0 {
		t.Fatal("the first presentation must ask for a scale")
	}
	for x := 11.0; x < 60; x++ {
		pane.X, pane.ClipX = x, x
		moved, _ := placePage(pane, 800, 600)
		engine.SetPageBounds("page1", moved)
	}
	if got := recorder.count(); got != asked {
		t.Fatalf("moving the pane asked for %d more scales", got-asked)
	}
	// Every move still reaches the launcher.
	bounds := 0
	for _, op := range sink.ops() {
		if op == webview2host.OpBounds {
			bounds++
		}
	}
	if bounds != 50 {
		t.Fatalf("sent %d bounds directives, want one per placement (50)", bounds)
	}
	// A resize that changes the scale is metrics work.
	pane.Width, pane.Height = 400, 300
	resized, _ := placePage(pane, 800, 600)
	engine.SetPageBounds("page1", resized)
	if got := recorder.count(); got != asked+1 {
		t.Fatalf("a scale change asked %d times, want once", got-asked)
	}
}

// The page's own pixel ratio is a CDP evaluate. It runs in the scale drain,
// so a presentation pass (which holds the thread's presentation) never waits
// on it.
func TestHostedPresentationNeverWaitsOnThePagePixelRatio(t *testing.T) {
	engine, _, _ := newHostedGeometryEngine(t)
	release := make(chan struct{})
	reading := make(chan struct{}, 1)
	engine.mu.Lock()
	engine.readPageRatio = func(ctx context.Context, _ *cdpPage) (float64, error) {
		reading <- struct{}{}
		select {
		case <-release:
			return 1.25, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	engine.mu.Unlock()
	pane := PaneRect{X: 0, Y: 0, Width: 640, Height: 360, ClipX: 0, ClipY: 0, ClipWidth: 640, ClipHeight: 360,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 3.75, Visible: true}
	placement, _ := placePage(pane, 1280, 720)
	returned := make(chan struct{})
	go func() {
		engine.SetPageBounds("page1", placement)
		engine.ShowPage("page1")
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("presenting waited on the page's pixel ratio")
	}
	select {
	case <-reading:
	case <-time.After(5 * time.Second):
		t.Fatal("the pixel ratio was never read")
	}
	close(release)
	// The drain then applies the zoomed scale: 0.5 x (3.75 / 1.25).
	deadline := time.Now().Add(5 * time.Second)
	page := engine.driverFor("page1")
	for {
		page.metricsMu.Lock()
		got := page.viewScale
		page.metricsMu.Unlock()
		if got == 1.5 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("view scale = %v, want 1.5", got)
		}
		time.Sleep(time.Millisecond)
	}
}

// The WebKitGTK overlay positions in whole pixels: edges snap with the shared
// rule from the same scaled coordinates, so a page scrolled partly off the
// left of the window keeps its width and lands on the pixel the positive side
// would round to (the glue truncated toward zero and rounded sizes on their
// own, which moved such a page by a pixel and shaved a column).
func TestOverlayPaneLayoutSnapsEdges(t *testing.T) {
	placement := PanePlacement{
		Rect: PaneRect{
			X: -5.8, Y: 3.3, Width: 400, Height: 300,
			ClipX: 0, ClipY: 3.3, ClipWidth: 394.2, ClipHeight: 300,
			ViewportWidth: 1280, ViewportHeight: 720,
		},
		Scale: 1,
	}
	layout, scale := overlayPaneLayout(placement, 1920, 1080)
	if scale != 1.5 {
		t.Fatalf("scale = %v, want the overlay proportion 1.5", scale)
	}
	// Page left -8.7 -> -9 (truncating -8.7 + 0.5 gave -8); page right
	// 591.3 -> 591 (a separately rounded width of 600 put it at 592).
	if layout.OffsetX != -9 || layout.Clip.Left != 0 || layout.Clip.Right != 591 {
		t.Fatalf("layout = %+v", layout)
	}
	if layout.Clip.Top != 5 || layout.Clip.Bottom != 455 || layout.OffsetY != 0 {
		t.Fatalf("vertical layout = %+v", layout)
	}
	// No viewport: the rect is already in overlay pixels.
	placement.Rect.ViewportWidth, placement.Rect.ViewportHeight = 0, 0
	placement.Scale = 0.5
	if _, scale := overlayPaneLayout(placement, 1920, 1080); scale != 0.5 {
		t.Fatalf("unscaled overlay scale = %v, want the placement's 0.5", scale)
	}
}

// The bounds directive names the mount and the page size, so a launcher
// holding newer geometry for that mount can place the page itself, and
// carries the pixel ratio the launcher converts CSS pixels with.
func TestHostedBoundsDirectiveNamesTheMountAndPage(t *testing.T) {
	engine, sink, _ := newHostedGeometryEngine(t)
	pane := PaneRect{X: 12, Y: 34, Width: 800, Height: 600, ClipX: 12, ClipY: 34, ClipWidth: 800, ClipHeight: 600,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 1.5, Visible: true}
	placement, _ := placePage(pane, 800, 600)
	placement.Mount = "mount-1"
	engine.SetPageBounds("page1", placement)
	d := sink.next(t)
	if d.Mount != "mount-1" || d.PageW != 800 || d.PageH != 600 || d.DPR != 1.5 {
		t.Fatalf("directive = %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("directive invalid: %v", err)
	}
}
