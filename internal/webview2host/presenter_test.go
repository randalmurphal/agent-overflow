package webview2host

import (
	"encoding/json"
	"testing"
)

// The backend's directive for a page following an 800x600 pane mounted as
// "mount-1", placed from the rect it last heard: x=10.
func boundsDirective() Directive {
	return Directive{
		Op: OpBounds, PageID: "page-1",
		X: 10, Y: 20, W: 800, H: 600, CX: 10, CY: 20, CW: 800, CH: 600,
		VW: 1600, VH: 900, DPR: 1.25, Bg: "#000000",
		Mount: "mount-1", PageW: 800, PageH: 600,
	}
}

func localGeometry(x float64) PaneGeometry {
	return PaneGeometry{
		Mount: "mount-1", Seq: 1,
		X: x, Y: 20, Width: 800.5, Height: 600.5,
		ClipX: x, ClipY: 20, ClipWidth: 800.5, ClipHeight: 600.5,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 1.5,
		Visible: true, Background: "#ffffff",
	}
}

// With local geometry for the page's mount, the page is placed from it with
// the shared rule and the directive's (older) position is ignored, so a
// directive arriving after the pane moved cannot pull the view back.
func TestPresenterPlacesFromLocalGeometry(t *testing.T) {
	var p pagePresentation
	p.noteBounds(boundsDirective())
	p.backendShown = true
	view := p.view(localGeometry(300), true)
	if !view.visible || !view.hasBounds {
		t.Fatalf("view = %+v", view)
	}
	b := view.bounds
	// A following page starts at the local rect's origin (paneplace.Fit) at
	// the directive's page size, and carries the local ratio and color.
	if b.X != 300 || b.Y != 20 || b.W != 800 || b.H != 600 || b.DPR != 1.5 || b.Bg != "#ffffff" || b.VW != 1600 {
		t.Fatalf("local placement = %+v", b)
	}
	if b.CX != 300 || b.CW != 800 {
		t.Fatalf("local clip = %+v", b)
	}
}

// Effective visibility is backend-shown AND locally visible.
func TestPresenterShowsOnlyWhenBothSidesSaySo(t *testing.T) {
	var p pagePresentation
	p.noteBounds(boundsDirective())
	local := localGeometry(10)
	if p.view(local, true).visible {
		t.Fatal("local geometry alone must not show a page the backend has not shown")
	}
	p.backendShown = true
	local.Visible = false
	if v := p.view(local, true); v.visible {
		t.Fatalf("a locally hidden pane must hide the page: %+v", v)
	}
	local = localGeometry(10)
	local.ClipWidth, local.ClipHeight = 0, 0
	if v := p.view(local, true); v.visible {
		t.Fatalf("a pane clipped away must hide the page: %+v", v)
	}
}

// Without local geometry the directive stands exactly as before: no mailbox
// entry, a mount the mailbox does not know, or a backend that predates the
// mount field (new launcher, old backend).
func TestPresenterFallsBackToTheDirective(t *testing.T) {
	d := boundsDirective()
	var p pagePresentation
	p.noteBounds(d)
	p.backendShown = true
	if v := p.view(PaneGeometry{}, false); v.bounds != d.paneBounds() || !v.visible {
		t.Fatalf("no local geometry: view = %+v", v)
	}
	other := localGeometry(300)
	other.Mount = "mount-2"
	if v := p.view(other, true); v.bounds != d.paneBounds() {
		t.Fatalf("another mount's geometry placed the page: %+v", v)
	}

	// An older backend's directive: no mount, no page size.
	var legacy Directive
	if err := json.Unmarshal([]byte(`{"op":"bounds","pageId":"page-1","x":10,"y":20,"w":800,"h":600,"vw":1600,"vh":900}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("an older backend's directive must stay valid: %v", err)
	}
	var old pagePresentation
	old.noteBounds(legacy)
	old.backendShown = true
	if v := old.view(localGeometry(300), true); v.bounds.X != 10 || !v.visible {
		t.Fatalf("an unnamed mount must use the directive: %+v", v)
	}
}

// The SPA numbers each report once for both paths, so the newer of the two
// places the page: a directive placed from a report the launcher never got
// locally (its post failed) wins over the older local report, and a local
// report at or past the directive's wins over it. A directive with no
// sequence (an older backend) is the case above: any local report wins.
func TestPresenterPlacesFromTheNewerReport(t *testing.T) {
	var d Directive
	if err := json.Unmarshal([]byte(`{"op":"bounds","pageId":"page-1","x":10,"y":20,"w":800,"h":600,"cx":10,"cy":20,"cw":800,"ch":600,"vw":1600,"vh":900,"mount":"mount-1","pageW":800,"pageH":600,"seq":5}`), &d); err != nil {
		t.Fatal(err)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	var p pagePresentation
	p.noteBounds(d)
	p.backendShown = true
	for _, tc := range []struct {
		seq   uint64
		wantX float64
	}{{4, 10}, {5, 300}, {6, 300}} {
		local := localGeometry(300)
		local.Seq = tc.seq
		v := p.view(local, true)
		if v.bounds.X != tc.wantX || !v.visible {
			t.Fatalf("local seq %d against directive seq 5: view = %+v, want x %g", tc.seq, v, tc.wantX)
		}
	}
	// A newer directive also decides visibility: a stale local report that
	// says hidden does not hide a page the backend shows from a newer one.
	stale := localGeometry(300)
	stale.Seq, stale.Visible = 4, false
	if v := p.view(stale, true); !v.visible {
		t.Fatalf("a stale local report hid the page: %+v", v)
	}
}

// An older launcher decodes the new directive into the fields it knows and
// ignores the rest; those fields alone must still place the page where the
// backend placed it.
func TestNewBoundsDirectiveDecodesForAnOlderLauncher(t *testing.T) {
	raw, err := json.Marshal(boundsDirective())
	if err != nil {
		t.Fatal(err)
	}
	var older struct {
		Op             Op
		PageID         string `json:"pageId"`
		X, Y, W, H     float64
		CX, CY, CW, CH float64
		VW, VH         float64
		Bg             string
	}
	if err := json.Unmarshal(raw, &older); err != nil {
		t.Fatalf("an older launcher cannot decode: %v", err)
	}
	if older.Op != OpBounds || older.X != 10 || older.W != 800 || older.CW != 800 || older.VW != 1600 {
		t.Fatalf("older decode = %+v", older)
	}
}

func TestNextPresentStepMovesBeforeShowingAndNeverMovesHidden(t *testing.T) {
	a := paneView{bounds: paneBounds{X: 1, W: 10, H: 10}, hasBounds: true, visible: true}
	b := paneView{bounds: paneBounds{X: 2, W: 10, H: 10}, hasBounds: true, visible: true}

	step, shown := nextPresentStep(paneView{}, a, false)
	if !step.move || step.bounds != a.bounds || !step.show || step.hide {
		t.Fatalf("first show = %+v", step)
	}
	// Unchanged: nothing to do, unless a backend show asks for a raise.
	if step, _ := nextPresentStep(shown, a, false); step != (presentStep{}) {
		t.Fatalf("unchanged view did work: %+v", step)
	}
	if step, _ := nextPresentStep(shown, a, true); !step.show || step.move {
		t.Fatalf("a raise must re-show without moving: %+v", step)
	}
	// A move while shown moves only.
	step, shown = nextPresentStep(shown, b, false)
	if !step.move || step.show || step.hide {
		t.Fatalf("move = %+v", step)
	}
	// Hidden: hide, and later geometry while hidden moves nothing.
	step, shown = nextPresentStep(shown, paneView{}, false)
	if !step.hide || step.move {
		t.Fatalf("hide = %+v", step)
	}
	hiddenMove := paneView{bounds: paneBounds{X: 3, W: 10, H: 10}, hasBounds: true}
	if step, _ := nextPresentStep(shown, hiddenMove, false); step != (presentStep{}) {
		t.Fatalf("a hidden page was touched: %+v", step)
	}
	// Re-shown at a new place: moved first, then shown.
	reshown := hiddenMove
	reshown.visible = true
	step, shown = nextPresentStep(shown, reshown, false)
	if !step.move || !step.show || step.bounds.X != 3 || !shown.visible {
		t.Fatalf("reshow = %+v", step)
	}
}

func TestDeviceScalePrefersThePixelRatio(t *testing.T) {
	// innerWidth is an integer: a 1919.5 CSS-px-wide SPA at 1.25 reports 1919,
	// and client/innerWidth misplaces a far edge by most of a pixel.
	b := paneBounds{VW: 1919, VH: 1079, DPR: 1.25}
	if sx, sy := deviceScale(b, 2399, 1349, true); sx != 1.25 || sy != 1.25 {
		t.Fatalf("scale = %v,%v, want the pixel ratio", sx, sy)
	}
	b.DPR = 0
	if sx, sy := deviceScale(b, 2399, 1349, true); sx != 2399.0/1919 || sy != 1349.0/1079 {
		t.Fatalf("no ratio must fall back to client over viewport, got %v,%v", sx, sy)
	}
	if sx, sy := deviceScale(b, 0, 0, false); sx != 1 || sy != 1 {
		t.Fatalf("no client size must mean client pixels, got %v,%v", sx, sy)
	}
	if sx, sy := deviceScale(paneBounds{}, 2399, 1349, true); sx != 1 || sy != 1 {
		t.Fatalf("no viewport must mean client pixels, got %v,%v", sx, sy)
	}
}

func TestBoundsDirectiveValidatesMountAndPageSize(t *testing.T) {
	if err := boundsDirective().Validate(); err != nil {
		t.Fatalf("a full directive must validate: %v", err)
	}
	for name, mutate := range map[string]func(*Directive){
		"mount with a space": func(d *Directive) { d.Mount = "a b" },
		"half page size":     func(d *Directive) { d.PageH = 0 },
		"negative page":      func(d *Directive) { d.PageW, d.PageH = -1, -1 },
		"huge page":          func(d *Directive) { d.PageW = 1e6 },
		"negative dpr":       func(d *Directive) { d.DPR = -1 },
		"zero width":         func(d *Directive) { d.W = 0 },
	} {
		d := boundsDirective()
		mutate(&d)
		if err := d.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
