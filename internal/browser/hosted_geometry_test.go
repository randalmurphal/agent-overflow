package browser

import (
	"context"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// hostedScaleRig is a tab of the hosted engine against the fake launcher:
// the view scales it is asked for are the device metrics overrides its
// CDP endpoint answers, and ratio stands in for the page's own pixel ratio.
type hostedScaleRig struct {
	l    *popupLauncher
	page ids.PageHandle
}

func newHostedScaleRig(t *testing.T, ratio func(context.Context, *cdpPage) (float64, error)) *hostedScaleRig {
	t.Helper()
	l := newPopupLauncher(t)
	l.engine.readPageRatio = ratio
	_, page := l.open(l.access("thread"))
	r := &hostedScaleRig{l: l, page: ids.PageHandle(page)}
	r.idle(t)
	return r
}

// overrides counts the device metrics overrides sent so far.
func (r *hostedScaleRig) overrides() int {
	return r.l.called("Emulation.setDeviceMetricsOverride")
}

// idle waits until no view scale is being applied to the page.
func (r *hostedScaleRig) idle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		r.l.engine.mu.Lock()
		st := r.l.engine.scales[r.page]
		busy := st != nil && st.busy
		r.l.engine.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the scale drain never went idle")
		}
		time.Sleep(time.Millisecond)
	}
}

// scale answers the scale the page's override draws it at.
func (r *hostedScaleRig) scale() float64 {
	r.l.engine.mu.Lock()
	page := r.l.engine.drivers[r.page]
	r.l.engine.mu.Unlock()
	page.metricsMu.Lock()
	defer page.metricsMu.Unlock()
	return page.viewScale
}

// newHostedBoundsEngine is an engine with one bound page whose directives
// sink records.
func newHostedBoundsEngine(t *testing.T) (*hostedEngine, *directiveSink) {
	t.Helper()
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	bindTestPage(t, engine, "page1", "TARGET-1")
	return engine, sink
}

func fixedRatio(ratio float64) func(context.Context, *cdpPage) (float64, error) {
	return func(context.Context, *cdpPage) (float64, error) { return ratio, nil }
}

// A pane that only moves (the strip scrolling, a neighbor's divider) sends
// the launcher its new place and asks the page for nothing: no device
// metrics override. A resize that changes the scale sends one.
func TestHostedPositionOnlyChangeDoesNoMetricsWork(t *testing.T) {
	r := newHostedScaleRig(t, fixedRatio(1.25))
	engine := r.l.engine
	before := r.overrides()
	pane := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ClipX: 10, ClipY: 20, ClipWidth: 800, ClipHeight: 600,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 1.25, Visible: true}
	placement, _ := placePage(pane, 800, 600)
	engine.SetPageBounds(r.page, placement)
	engine.ShowPage(r.page)
	r.idle(t)
	shown := r.overrides()
	if shown == before {
		t.Fatal("the first presentation sent no device metrics override")
	}
	for x := 11.0; x < 60; x++ {
		pane.X, pane.ClipX = x, x
		moved, _ := placePage(pane, 800, 600)
		engine.SetPageBounds(r.page, moved)
	}
	r.idle(t)
	if got := r.overrides(); got != shown {
		t.Fatalf("moving the pane sent %d device metrics overrides", got-shown)
	}
	pane.Width, pane.Height = 400, 300
	resized, _ := placePage(pane, 800, 600)
	engine.SetPageBounds(r.page, resized)
	r.idle(t)
	if got := r.overrides(); got != shown+1 {
		t.Fatalf("a scale change sent %d device metrics overrides, want one", got-shown)
	}
}

// Every move of a pane still reaches the launcher as its bounds.
func TestHostedEveryPlacementReachesTheLauncher(t *testing.T) {
	engine, sink := newHostedBoundsEngine(t)
	pane := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ClipX: 10, ClipY: 20, ClipWidth: 800, ClipHeight: 600,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 1.25, Visible: true}
	for x := 10.0; x < 60; x++ {
		pane.X, pane.ClipX = x, x
		moved, _ := placePage(pane, 800, 600)
		engine.SetPageBounds("page1", moved)
	}
	bounds := 0
	for _, op := range sink.ops() {
		if op == webview2host.OpBounds {
			bounds++
		}
	}
	if bounds != 50 {
		t.Fatalf("sent %d bounds directives, want one per placement (50)", bounds)
	}
}

// A scale that did not apply (here the page's pixel ratio could not be read)
// is not remembered, so the next placement asks for it again rather than the
// dedupe leaving the page at the wrong zoom.
func TestHostedFailedScaleIsAskedAgain(t *testing.T) {
	var mu sync.Mutex
	fail := true
	r := newHostedScaleRig(t, func(context.Context, *cdpPage) (float64, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return 0, context.DeadlineExceeded
		}
		return 1.25, nil
	})
	engine := r.l.engine
	pane := PaneRect{X: 0, Y: 0, Width: 640, Height: 360, ClipX: 0, ClipY: 0, ClipWidth: 640, ClipHeight: 360,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 2.5, Visible: true}
	placement, _ := placePage(pane, 640, 360)
	engine.SetPageBounds(r.page, placement)
	engine.ShowPage(r.page)
	r.idle(t)
	mu.Lock()
	fail = false
	mu.Unlock()
	pane.X, pane.ClipX = 5, 5
	moved, _ := placePage(pane, 640, 360)
	engine.SetPageBounds(r.page, moved)
	r.idle(t)
	if got := r.scale(); got != 2 {
		t.Fatalf("view scale = %v, want 1 x (2.5 / 1.25)", got)
	}
}

// The page's own pixel ratio is a CDP evaluate. It runs in the scale drain,
// so a presentation pass (which holds the thread's presentation) never waits
// on it.
func TestHostedPresentationNeverWaitsOnThePagePixelRatio(t *testing.T) {
	release := make(chan struct{})
	reading := make(chan struct{}, 1)
	r := newHostedScaleRig(t, func(ctx context.Context, _ *cdpPage) (float64, error) {
		select {
		case reading <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return 1.25, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})
	engine := r.l.engine
	pane := PaneRect{X: 0, Y: 0, Width: 640, Height: 360, ClipX: 0, ClipY: 0, ClipWidth: 640, ClipHeight: 360,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 3.75, Visible: true}
	placement, _ := placePage(pane, 1280, 720)
	returned := make(chan struct{})
	go func() {
		engine.SetPageBounds(r.page, placement)
		engine.ShowPage(r.page)
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
	r.idle(t)
	if got := r.scale(); got != 1.5 {
		t.Fatalf("view scale = %v, want 1.5", got)
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
	layout, scale, ok := overlayPaneLayout(placement, 1920, 1080)
	if !ok || scale != 1.5 {
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
	if _, scale, _ := overlayPaneLayout(placement, 1920, 1080); scale != 0.5 {
		t.Fatalf("unscaled overlay scale = %v, want the placement's 0.5", scale)
	}
}

// The bounds directive names the mount, the page size and the sequence of
// the report it was placed from, so a launcher holding newer geometry for
// that mount can place the page itself, and carries the pixel ratio the
// launcher converts CSS pixels with.
func TestHostedBoundsDirectiveNamesTheMountAndPage(t *testing.T) {
	engine, sink := newHostedBoundsEngine(t)
	pane := PaneRect{X: 12, Y: 34, Width: 800, Height: 600, ClipX: 12, ClipY: 34, ClipWidth: 800, ClipHeight: 600,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 1.5, Visible: true, Seq: 7}
	placement, _ := placePage(pane, 800, 600)
	placement.Mount = "mount-1"
	engine.SetPageBounds("page1", placement)
	d := sink.next(t)
	if d.Mount != "mount-1" || d.PageW != 800 || d.PageH != 600 || d.DPR != 1.5 || d.Seq != 7 {
		t.Fatalf("directive = %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("directive invalid: %v", err)
	}
}

// The Manager keeps a report's sequence with the mount's rect, so the
// placement an engine is given names the report it was placed from.
func TestPanePlacementCarriesTheReportSequence(t *testing.T) {
	f := newPaneFollowFixture(t)
	mount := f.attach(t)
	rect := paneRect(800, 600)
	rect.Seq = 9
	if err := f.manager.SetPaneRect(MountID(mount), rect); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	f.awaitActiveAt(t, 800, 600)
	f.awaitFollowIdle(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.placements) == 0 {
		t.Fatal("the page was never placed")
	}
	if last := f.placements[len(f.placements)-1]; last.Rect.Seq != 9 || last.Mount != MountID(mount) {
		t.Fatalf("placement = %+v, want the report's sequence 9 in mount %s", last, mount)
	}
}
