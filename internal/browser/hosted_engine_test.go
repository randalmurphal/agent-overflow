package browser

import (
	"context"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
)

func TestHostedEngineStartRequiresWiring(t *testing.T) {
	engine := newHostedEngine(nil, nil, nil, engineEvents{})
	if err := engine.Start(context.Background()); err == nil {
		t.Fatal("an unwired pane host started")
	}
	if engine.Running() {
		t.Fatal("an unwired pane host reports running")
	}

	wired, _ := newTestHostedEngine(t, stubRelay{url: "ws://127.0.0.1:1/x"}, engineEvents{})
	if err := wired.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !wired.Running() {
		t.Fatal("a wired pane host does not report running")
	}
}

func TestHostedEngineRetiresAPageOnTheLauncherReports(t *testing.T) {
	// A process failure is a crash, not a close (crash_test.go).
	for _, kind := range []webview2host.ReportKind{webview2host.ReportClosed} {
		t.Run(string(kind), func(t *testing.T) {
			var closed []string
			engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{
				PageClosed: func(handle ids.PageHandle) { closed = append(closed, string(handle)) },
			})
			bindTestPage(t, engine, "page1", "TARGET-1")

			engine.Report("page1", kind, "")
			settleReports(t, engine, "")
			if len(closed) != 1 || closed[0] != "page1" {
				t.Fatalf("PageClosed saw %v", closed)
			}
			// The retirement is the engine's whole bookkeeping, so a repeat
			// report must not reach the Manager twice.
			engine.Report("page1", kind, "")
			settleReports(t, engine, "")
			if len(closed) != 1 {
				t.Fatalf("a repeated report was reported again: %v", closed)
			}
		})
	}
}

func TestHostedEngineShowAndHideAreDedupedAndPageScoped(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	bindTestPage(t, engine, "page1", "TARGET-1")

	engine.ShowPage("page1")
	engine.ShowPage("page1")
	engine.HidePage("page1")
	engine.HidePage("page1")
	engine.ShowPage("page1")
	// A handle the engine does not own is not a directive: the launcher
	// would refuse it, and the Manager's registry is the authority.
	engine.ShowPage("page2")
	engine.HidePage("page2")

	sink.expectOps(t, webview2host.OpShow, webview2host.OpHide, webview2host.OpShow)
}

func TestHostedEngineBoundsAndDevToolsEmitValidDirectives(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	bindTestPage(t, engine, "page1", "TARGET-1")
	// An 800x600 pane showing a 1280x720 page: the placement is the fitted,
	// centered rect (scale 0.625 -> 800x450, 75px down) and that is what the
	// launcher positions the controller at.
	pane := PaneRect{X: 12, Y: 34, Width: 800, Height: 600, ClipX: 12, ClipY: 34, ClipWidth: 800, ClipHeight: 600, ViewportWidth: 1600, ViewportHeight: 900, Visible: true}
	placement, ok := placePage(pane, 1280, 720)
	if !ok {
		t.Fatal("an 800x600 pane must place a 1280x720 page")
	}
	engine.SetPageBounds("page1", placement)
	if err := engine.OpenPageDevTools("page1"); err != nil {
		t.Fatal(err)
	}
	if err := engine.OpenPageDevTools(""); err == nil {
		t.Fatal("a devtools directive the launcher would refuse was reported sent")
	}
	sink.expectOps(t, webview2host.OpBounds, webview2host.OpDevTools)

	bounds := sink.next(t)
	if bounds.X != 12 || bounds.Y != 109 || bounds.W != 800 || bounds.H != 450 || bounds.VW != 1600 || bounds.VH != 900 {
		t.Fatalf("bounds directive lost its fitted rectangle: %+v", bounds)
	}
	// The presentation sync re-sends the active rect on every selection and
	// page-list change; an unmoved rect costs no directive.
	engine.SetPageBounds("page1", placement)
	sink.expectOps(t, webview2host.OpBounds, webview2host.OpDevTools)
	// A page this engine does not own is not a directive, same as show/hide.
	other, _ := placePage(PaneRect{X: 1, Y: 2, Width: 300, Height: 400, ClipX: 1, ClipY: 2, ClipWidth: 300, ClipHeight: 400}, 300, 400)
	engine.SetPageBounds("page2", other)
	sink.expectOps(t, webview2host.OpBounds, webview2host.OpDevTools)
	// A rectangle the launcher would refuse never reaches the wire.
	engine.SetPageBounds("page1", PanePlacement{})
	sink.expectOps(t, webview2host.OpBounds, webview2host.OpDevTools)
}

// A page's base reaches the launcher on its own directive, with no bounds
// and no show before it, and a placement carries no color.
func TestHostedEngineSendsAPageBaseApartFromItsPlacement(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	bindTestPage(t, engine, "page1", "TARGET-1")
	engine.SetPageBase("page1", "#1e1e2e")
	sink.expectOps(t, webview2host.OpBackground)
	if base := sink.next(t); base.PageID != "page1" || base.Bg != "#1e1e2e" {
		t.Fatalf("background directive = %+v", base)
	}
	// A page this engine does not own, and a color the launcher would
	// refuse, never reach the wire.
	engine.SetPageBase("page2", "#ffffff")
	engine.SetPageBase("page1", "red")
	sink.expectOps(t, webview2host.OpBackground)

	pane := PaneRect{X: 12, Y: 34, Width: 800, Height: 600, ClipX: 12, ClipY: 34, ClipWidth: 800, ClipHeight: 600, ViewportWidth: 1600, ViewportHeight: 900, Visible: true, Background: "#123456"}
	placement, ok := placePage(pane, 800, 600)
	if !ok {
		t.Fatal("an 800x600 pane must place an 800x600 page")
	}
	engine.SetPageBounds("page1", placement)
	sink.expectOps(t, webview2host.OpBackground, webview2host.OpBounds)
	if bounds := sink.next(t); bounds.Bg != "" {
		t.Fatalf("a bounds directive carries the color %q", bounds.Bg)
	}
}

func TestHostedEngineRefusesAnInvalidPageID(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	bindTestPage(t, engine, "not a page id", "TARGET-1")
	engine.ShowPage("not a page id")
	engine.closePage("not a page id")
	sink.expectOps(t)
}

func TestHostedEngineRekeysBrowserEventsOntoPageIDs(t *testing.T) {
	var infos []string
	// targetDestroyed is reported off the CDP event goroutine, so the
	// closure it lands in has to be synchronized like the real one.
	closed := make(chan string, 4)
	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{
		PageClosed: func(handle ids.PageHandle) { closed <- string(handle) },
		PageInfoChanged: func(handle ids.PageHandle, url, title string, order uint64) {
			infos = append(infos, fmt.Sprintf("%s %s %s %d", handle, url, title, order))
		},
	})
	bindTestPage(t, engine, "page1", "TARGET-1")

	engine.targetInfoChanged(engine.currentTargets(), target.EventTargetInfoChanged{TargetInfo: &target.Info{
		TargetID: "TARGET-1", Type: "page", URL: "https://example.test/", Title: "Example",
	}})
	// The reports are numbered in the browser's order, which the Manager
	// keeps the highest of.
	engine.targetInfoChanged(engine.currentTargets(), target.EventTargetInfoChanged{TargetInfo: &target.Info{
		TargetID: "TARGET-1", Type: "page", URL: "https://example.test/", Title: "Retitled",
	}})
	if len(infos) != 2 || infos[0] != "page1 https://example.test/ Example 1" || infos[1] != "page1 https://example.test/ Retitled 2" {
		t.Fatalf("PageInfoChanged saw %v", infos)
	}
	// A target this engine never created is dropped rather than reported
	// under a handle the Manager does not own.
	engine.targetInfoChanged(engine.currentTargets(), target.EventTargetInfoChanged{TargetInfo: &target.Info{
		TargetID: "TARGET-9", Type: "page", URL: "https://elsewhere.test/",
	}})
	if len(infos) != 2 {
		t.Fatalf("an unknown target was reported: %v", infos)
	}

	// The launcher's close ends a page whose target went; with none, the
	// destroy does at the fallback bound.
	engine.closeFallback = 100 * time.Millisecond
	destroyed := time.Now()
	engine.targetDestroyed(engine.currentTargets(), target.EventTargetDestroyed{TargetID: "TARGET-1"})
	select {
	case handle := <-closed:
		if handle != "page1" {
			t.Fatalf("PageClosed saw %q", handle)
		}
		if waited := time.Since(destroyed); waited < engine.closeFallback {
			t.Fatalf("the page was retired %v after its target went, before the launcher's close could come", waited)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("targetDestroyed did not retire the page")
	}
}

func TestHostedEngineDiscardPageClosesTheController(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	if err := engine.DiscardPage(t.Context(), "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	sink.expectOps(t, webview2host.OpClose)
}

// The pane presentation path the companion drives is an engine
// capability, not a Manager branch: only the hosted engine answers it.
var _ paneHost = (*hostedEngine)(nil)

func TestManagerSelectsTheHostedEngineFromThePaneHostOptions(t *testing.T) {
	sink := newDirectiveSink()
	manager := NewManager(t.TempDir(), Config{}, ManagerOptions{PaneHost: &PaneHostOptions{
		Directive: sink.send,
		Relay:     stubRelay{},
	}})
	if _, ok := manager.engine.(*hostedEngine); !ok {
		t.Fatalf("engine is %T, want the hosted engine", manager.engine)
	}

	if err := manager.ReportPaneHost("not a page id", webview2host.ReportClosed, ""); err == nil {
		t.Fatal("an invalid page id was routed to the engine")
	}
	if err := manager.ReportPaneHost("page1", "nonsense", ""); err == nil {
		t.Fatal("an unknown report kind was routed to the engine")
	}
	if err := manager.ReportPaneHost("page1", webview2host.ReportCreated, "TARGET-1"); err != nil {
		t.Fatalf("route a created report: %v", err)
	}
	// Nobody was waiting on it, so the engine closes the orphan.
	sink.expectOps(t, webview2host.OpClose)
}

// A windowless deployment has NO engine. That is the whole story spec §9
// leaves behind: no pane host, no native window, no fallback browser to
// quietly launch — a browser tool call gets one sentence saying so.
func TestManagerWithoutAWindowHasNoEngineAtAll(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{}, ManagerOptions{})
	if _, ok := manager.engine.(unavailableEngine); !ok {
		t.Fatalf("engine is %T, want no engine", manager.engine)
	}
	if manager.Available() {
		t.Fatal("a windowless deployment reported browser tools as available")
	}
	if err := manager.ReportPaneHost("page1", webview2host.ReportClosed, ""); err == nil {
		t.Fatal("a deployment with no pane host accepted a report")
	}
}

// A file the agent opens is on the WSL filesystem, but the renderer that
// must load it is a Windows-side WebView2, so the URL has to carry the
// Windows view of the path: file:///home/... answers ERR_FILE_NOT_FOUND
// there. The engine advertises that through
// engineFileURL; losing the implementation would silently regress
// browser_open_file to a URL no Windows renderer can read.
var _ engineFileURL = (*hostedEngine)(nil)

func TestWindowsFileURLCarriesTheRendererView(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
	}{
		{"wsl UNC", `\\wsl.localhost\AlmaLinux-10\home\u\page.html`, "file://wsl.localhost/AlmaLinux-10/home/u/page.html"},
		{"drive path", `C:\Users\u\page.html`, "file:///C:/Users/u/page.html"},
		{"drive path with spaces", `C:\My Files\a b.html`, "file:///C:/My%20Files/a%20b.html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := windowsFileURL(tc.path)
			if err != nil || got != tc.want {
				t.Fatalf("windowsFileURL(%q) = %q, %v; want %q", tc.path, got, err, tc.want)
			}
		})
	}
	for _, path := range []string{"", `\\hostonly`, `\\wsl.localhost`, "relative/path", "/linux/path"} {
		if got, err := windowsFileURL(path); err == nil {
			t.Errorf("windowsFileURL(%q) = %q, want an error", path, got)
		}
	}
}

// The interceptor and the companion address bar hand back renderer-form
// file URLs; windowsPathFromFileURL must invert windowsFileURL exactly, or
// the authority check blocks the navigation OpenFile just authorized with
// ERR_BLOCKED_BY_CLIENT.
func TestWindowsPathFromFileURLInvertsTheRendererView(t *testing.T) {
	for _, tc := range []struct {
		name, rawURL, want string
	}{
		{"wsl UNC", "file://wsl.localhost/AlmaLinux-10/home/u/page.html", `\\wsl.localhost\AlmaLinux-10\home\u\page.html`},
		{"drive path", "file:///C:/Users/u/page.html", `C:\Users\u\page.html`},
		{"drive path with spaces", "file:///C:/My%20Files/a%20b.html", `C:\My Files\a b.html`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := windowsPathFromFileURL(tc.rawURL)
			if err != nil || got != tc.want {
				t.Fatalf("windowsPathFromFileURL(%q) = %q, %v; want %q", tc.rawURL, got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct{ name, rawURL string }{
		{"not file", "https://example.test/a"},
		{"empty", ""},
		{"UNC host without a share", "file://wsl.localhost"},
		{"hostless without a drive", "file:///home/u/page.html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := windowsPathFromFileURL(tc.rawURL); err == nil {
				t.Fatalf("windowsPathFromFileURL(%q) = %q, want an error", tc.rawURL, got)
			}
		})
	}
	for _, path := range []string{`\\wsl.localhost\AlmaLinux-10\home\u\a b.html`, `C:\My Files\a b.html`} {
		asURL, err := windowsFileURL(path)
		if err != nil {
			t.Fatalf("windowsFileURL(%q): %v", path, err)
		}
		back, err := windowsPathFromFileURL(asURL)
		if err != nil || back != path {
			t.Fatalf("round trip of %q via %q = %q, %v", path, asURL, back, err)
		}
	}
}

// The launcher matches chords in its own process, so the backend's whole
// part is: ship the set at Start (and again whenever it changes), and route
// the report of a match to the Manager as the page's KeyChord.
func TestHostedEngineShipsAcceleratorsAndRoutesAMatch(t *testing.T) {
	set := keybindings.AcceleratorSet{{Key: "w", Ctrl: true}: {}}
	var chords []keybindings.Accelerator
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{
		KeyChord: func(handle ids.PageHandle, pressed keybindings.Accelerator, _ keyPress) bool {
			if handle != "page1" {
				t.Errorf("KeyChord for %q, want page1", handle)
			}
			chords = append(chords, pressed)
			return true
		},
	})
	engine.accelerators = func() keybindings.AcceleratorSet { return set }
	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	engine.SyncAccelerators()
	sink.expectOps(t, webview2host.OpAccelerators, webview2host.OpAccelerators)
	if got := sink.all[0].Accelerators; len(got) != 1 || got[0] != (keybindings.Accelerator{Key: "w", Ctrl: true}) {
		t.Fatalf("shipped set = %#v", got)
	}

	engine.Report("page1", webview2host.ReportAccelerator, `{"key":"w","ctrl":true}`)
	engine.Report("page1", webview2host.ReportAccelerator, `not json`)
	if len(chords) != 1 || chords[0] != (keybindings.Accelerator{Key: "w", Ctrl: true}) {
		t.Fatalf("KeyChord saw %#v", chords)
	}
}

// A pane drag asks for a scale per frame. Each is a CDP round trip, so they
// must collapse: the override ends at the LAST scale asked for, never queued
// through every intermediate one after the drag has stopped.
func TestHostedEngineViewScaleIsLatestWins(t *testing.T) {
	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	bindTestPage(t, engine, "page1", "TARGET-1")
	// No viewport yet, so SetViewScale records the scale without a CDP call.
	page := &cdpPage{ctx: t.Context()}
	engine.mu.Lock()
	engine.drivers["page1"] = page
	engine.mu.Unlock()

	for i := 1; i <= 50; i++ {
		engine.applyViewScale("page1", viewScale{placement: float64(i) / 100})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		engine.mu.Lock()
		busy := engine.scales["page1"].busy
		engine.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scale drain never went idle")
		}
		time.Sleep(time.Millisecond)
	}
	page.metricsMu.Lock()
	got := page.viewScale
	page.metricsMu.Unlock()
	if got != 0.5 {
		t.Fatalf("the page must end at the last scale asked for, got %v", got)
	}
	// A page this engine no longer owns gets no drain at all.
	engine.applyViewScale("page2", viewScale{placement: 0.25})
	engine.mu.Lock()
	_, queued := engine.scales["page2"]
	engine.mu.Unlock()
	if queued {
		t.Fatal("an unknown page must not be queued")
	}
}

// A hide puts the override back to 1, and a re-show at the SAME placement
// sends no bounds directive, so the show itself must restore the placement's
// scale. Losing that drew the page 1:1, cropped by the pane, until a resize.
func TestHostedEngineReShowRestoresThePlacementScale(t *testing.T) {
	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	bindTestPage(t, engine, "page1", "TARGET-1")
	page := &cdpPage{ctx: t.Context()}
	engine.mu.Lock()
	engine.drivers["page1"] = page
	engine.mu.Unlock()
	scaleOf := func() float64 {
		deadline := time.Now().Add(5 * time.Second)
		for {
			engine.mu.Lock()
			st := engine.scales["page1"]
			busy := st != nil && st.busy
			engine.mu.Unlock()
			if !busy {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the scale drain never went idle")
			}
			time.Sleep(time.Millisecond)
		}
		page.metricsMu.Lock()
		defer page.metricsMu.Unlock()
		return page.viewScale
	}

	pane := PaneRect{X: 0, Y: 0, Width: 640, Height: 360, ClipX: 0, ClipY: 0, ClipWidth: 640, ClipHeight: 360, ViewportWidth: 1600, ViewportHeight: 900, Visible: true}
	placement, _ := placePage(pane, 1280, 720)
	engine.SetPageBounds("page1", placement)
	engine.ShowPage("page1")
	if got := scaleOf(); got != 0.5 {
		t.Fatalf("shown page must draw at the placement scale, got %v", got)
	}
	engine.HidePage("page1")
	if got := scaleOf(); got != 1 {
		t.Fatalf("hidden page must draw at 1, got %v", got)
	}
	engine.SetPageBounds("page1", placement)
	engine.ShowPage("page1")
	if got := scaleOf(); got != 0.5 {
		t.Fatalf("re-show at an unchanged placement must restore its scale, got %v", got)
	}
}
