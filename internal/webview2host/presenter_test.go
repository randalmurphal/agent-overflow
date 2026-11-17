package webview2host

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The backend's directive for a page following an 800x600 pane mounted as
// "mount-1", placed from the rect it last heard: x=10.
func boundsDirective() Directive {
	return Directive{
		Op: OpBounds, PageID: "page-1",
		X: 10, Y: 20, W: 800, H: 600, CX: 10, CY: 20, CW: 800, CH: 600,
		VW: 1600, VH: 900, DPR: 1.25,
		Mount: "mount-1", PageW: 800, PageH: 600,
	}
}

func localGeometry(x float64) PaneGeometry {
	return PaneGeometry{
		Mount: "mount-1", Seq: 1,
		X: x, Y: 20, Width: 800.5, Height: 600.5,
		ClipX: x, ClipY: 20, ClipWidth: 800.5, ClipHeight: 600.5,
		ViewportWidth: 1600, ViewportHeight: 900, DevicePixelRatio: 1.5,
		Visible: true,
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
	// the directive's page size, and carries the local ratio and the
	// directive's base color: the base is the page's, decided from its
	// address by the backend, never the mount's.
	if b.X != 300 || b.Y != 20 || b.W != 800 || b.H != 600 || b.DPR != 1.5 || b.VW != 1600 {
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

// A focus moves keyboard focus into a page only while the host shows it and
// the launcher window is the foreground window: AO never takes the OS focus
// from another application, and never focuses a hidden or unplaced view.
func TestFocusDecisionNeedsAShownPageAndTheForeground(t *testing.T) {
	shown := paneView{bounds: paneBounds{W: 10, H: 10}, hasBounds: true, visible: true}
	for _, tc := range []struct {
		name       string
		view       paneView
		foreground bool
		want       focusOutcome
	}{
		{"shown, foreground", shown, true, focusMove},
		{"shown, another app in front", shown, false, focusNotForeground},
		{"hidden", paneView{bounds: shown.bounds, hasBounds: true}, true, focusNotShown},
		{"never placed", paneView{visible: true}, true, focusNotShown},
		{"nothing", paneView{}, false, focusNotShown},
	} {
		taken := false
		take := func() bool { taken = true; return true }
		if got := focusDecision(tc.view, tc.foreground, take); got != tc.want {
			t.Errorf("%s: focusDecision = %v, want %v", tc.name, got, tc.want)
		}
		// Only an eligible page takes its pending intent; an ineligible one
		// keeps it for its next show.
		if taken != (tc.want == focusMove) {
			t.Errorf("%s: taken %v", tc.name, taken)
		}
	}
}

// launcherFocus is the launcher's focus path for one page, as Host runs it:
// a directive delivers its intent, and every decision (the directive's own,
// each later show of the page) asks focusDecision with Take.
type launcherFocus struct {
	f    *FocusIntents
	page string
}

func (l launcherFocus) decide(view paneView, foreground bool) (focusOutcome, uint64) {
	var intent uint64
	outcome := focusDecision(view, foreground, func() bool {
		var ok bool
		_, intent, ok = l.f.Take(l.page)
		return ok
	})
	return outcome, intent
}

var (
	focusShown  = paneView{bounds: paneBounds{W: 10, H: 10}, hasBounds: true, visible: true}
	focusHidden = paneView{bounds: paneBounds{W: 10, H: 10}, hasBounds: true}
)

// A focus directive delayed on its way through WSL leaves the keyboard where
// the user put it after asking: the SPA's withdrawal reaches the launcher
// first, straight from the SPA, and the launcher never applies the directive.
func TestALauncherFocusWithdrawnBeforeItsDirectiveArrivesIsDropped(t *testing.T) {
	arm := FocusIntentMessage{Mount: "mount-1", Intent: 1}
	withdraw := FocusIntentMessage{Mount: "mount-1", Intent: 1, Withdrawn: true}
	for _, tc := range []struct {
		name     string
		messages []FocusIntentMessage
	}{
		// Each is the SPA withdrawing on the user's next focus choice.
		{"the composer focused", []FocusIntentMessage{arm, withdraw}},
		{"the address bar refocused", []FocusIntentMessage{arm, withdraw}},
		{"a tab switch", []FocusIntentMessage{arm, withdraw}},
		// Messages arrive on goroutines of their own.
		{"the withdrawal heard before its arm", []FocusIntentMessage{withdraw, arm}},
		// A newer request supersedes the delayed one.
		{"a newer request armed", []FocusIntentMessage{arm, {Mount: "mount-1", Intent: 2}}},
	} {
		l := launcherFocus{f: NewFocusIntents(), page: "page-1"}
		for _, msg := range tc.messages {
			l.f.Note(msg)
		}
		l.f.Deliver("mount-1", 1, "page-1")
		if got, _ := l.decide(focusShown, true); got != focusNoIntent {
			t.Errorf("%s: the delayed directive = %v, want it dropped", tc.name, got)
		}
	}

	// A live intent moves focus once, and a directive that arrives before
	// its arm moves it too.
	l := launcherFocus{f: NewFocusIntents(), page: "page-1"}
	l.f.Note(arm)
	l.f.Deliver("mount-1", 1, "page-1")
	if got, intent := l.decide(focusShown, true); got != focusMove || intent != 1 {
		t.Fatalf("a live intent = %v (%d), want a move for intent 1", got, intent)
	}
	if got, _ := l.decide(focusShown, true); got != focusNoIntent {
		t.Fatalf("an applied intent = %v, want it applied once", got)
	}
	l.f.Deliver("mount-1", 2, "page-1")
	l.f.Note(FocusIntentMessage{Mount: "mount-1", Intent: 2})
	if got, intent := l.decide(focusShown, true); got != focusMove || intent != 2 {
		t.Fatalf("a directive heard before its arm = %v (%d), want a move for intent 2", got, intent)
	}
}

// Delivery is not application: a directive whose page the launcher's own
// presentation hides (a promote or demote moving the pane) waits, and the
// page's next show applies it once, unless it was withdrawn, replaced or
// outlived its age meanwhile.
func TestALauncherFocusWaitsForThePagesNextShow(t *testing.T) {
	now := time.Unix(1000, 0)
	newFocus := func() launcherFocus {
		f := NewFocusIntents()
		f.now = func() time.Time { return now }
		f.Note(FocusIntentMessage{Mount: "mount-1", Intent: 1})
		f.Deliver("mount-1", 1, "page-1")
		return launcherFocus{f: f, page: "page-1"}
	}

	l := newFocus()
	if got, _ := l.decide(focusHidden, true); got != focusNotShown {
		t.Fatalf("hidden = %v, want it left alone", got)
	}
	// Another page's show does not take it.
	if _, _, ok := l.f.Take("page-2"); ok {
		t.Fatal("another page took the intent")
	}
	// The window in the background keeps it waiting too.
	if got, _ := l.decide(focusShown, false); got != focusNotForeground {
		t.Fatalf("in the background = %v", got)
	}
	if got, intent := l.decide(focusShown, true); got != focusMove || intent != 1 {
		t.Fatalf("the next show = %v (%d), want the waiting intent applied", got, intent)
	}
	if got, _ := l.decide(focusShown, true); got != focusNoIntent {
		t.Fatalf("a later show = %v, want the intent applied once", got)
	}

	for _, tc := range []struct {
		name     string
		meantime func(l launcherFocus)
	}{
		{"withdrawn", func(l launcherFocus) { l.f.Note(FocusIntentMessage{Mount: "mount-1", Intent: 1, Withdrawn: true}) }},
		{"replaced", func(l launcherFocus) { l.f.Note(FocusIntentMessage{Mount: "mount-1", Intent: 2}) }},
		{"too old", func(launcherFocus) { now = now.Add(FocusIntentMaxAge + time.Second) }},
	} {
		now = time.Unix(1000, 0)
		l := newFocus()
		if got, _ := l.decide(focusHidden, true); got != focusNotShown {
			t.Fatalf("%s: hidden = %v", tc.name, got)
		}
		tc.meantime(l)
		if got, _ := l.decide(focusShown, true); got != focusNoIntent {
			t.Errorf("%s while waiting: the next show = %v, want nothing applied", tc.name, got)
		}
	}
}

// The record is bounded like the geometry mailbox, evicting the least
// recently updated mount.
func TestFocusIntentsAreBounded(t *testing.T) {
	f := NewFocusIntents()
	for i := range MaxGeometryMounts + 1 {
		f.Note(FocusIntentMessage{Mount: fmt.Sprintf("mount-%d", i), Intent: 1})
	}
	if len(f.entries) != MaxGeometryMounts {
		t.Fatalf("%d mounts kept, want %d", len(f.entries), MaxGeometryMounts)
	}
	if _, ok := f.entries["mount-0"]; ok {
		t.Fatal("the oldest mount was kept")
	}
}

func TestParseFocusIntentMessage(t *testing.T) {
	msg, err := ParseFocusIntentMessage(FocusMessagePrefix + `{"mount":"m-1","intent":2,"withdrawn":true,"extra":1}`)
	if err != nil || msg != (FocusIntentMessage{Mount: "m-1", Intent: 2, Withdrawn: true}) {
		t.Fatalf("parse = %+v, %v", msg, err)
	}
	for _, bad := range []string{
		FocusMessagePrefix + `{"mount":"m-1","intent":0}`,
		FocusMessagePrefix + `{"mount":"a/b","intent":1}`,
		FocusMessagePrefix + `{"intent":1}`,
		FocusMessagePrefix + `not json`,
		FocusMessagePrefix + `{"mount":"` + strings.Repeat("m", maxFocusMessageBytes) + `","intent":1}`,
	} {
		if _, err := ParseFocusIntentMessage(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := ParseFocusIntentMessage(GeometryMessagePrefix + `{}`); !errors.Is(err, ErrNotFocusIntent) {
		t.Fatalf("a geometry message = %v, want ErrNotFocusIntent", err)
	}
	report := FocusReport{Mount: "m-1", Intent: 4, Error: "E_FAIL"}
	if got, err := ParseFocusReport(report.Encode()); err != nil || got != report {
		t.Fatalf("report round trip = %+v, %v", got, err)
	}
	if _, err := ParseFocusReport(`{"mount":"m-1"}`); err == nil {
		t.Fatal("a report naming no intent parsed")
	}
	for _, kind := range []ReportKind{ReportFocused, ReportFocusFailed} {
		if !ValidKind(kind) {
			t.Errorf("%s is not a kind the backend accepts", kind)
		}
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
