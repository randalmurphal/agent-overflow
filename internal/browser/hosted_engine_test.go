package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// The hosted engine is exercised against a fake directive sink and a fake
// report source. Nothing here reaches a launcher, a WebView2, or a CDP
// endpoint: the relay is a stub, and every failure path below is one the
// engine must answer with an error rather than a hang.

const hostedTestDeadline = 3 * time.Second

// directiveSink is the launcher's inbox.
type directiveSink struct {
	mu   sync.Mutex
	all  []webview2host.Directive
	feed chan webview2host.Directive
}

func newDirectiveSink() *directiveSink {
	return &directiveSink{feed: make(chan webview2host.Directive, 64)}
}

func (s *directiveSink) send(directive webview2host.Directive) {
	s.mu.Lock()
	s.all = append(s.all, directive)
	s.mu.Unlock()
	select {
	case s.feed <- directive:
	default:
	}
}

func (s *directiveSink) ops() []webview2host.Op {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops := make([]webview2host.Op, 0, len(s.all))
	for _, directive := range s.all {
		ops = append(ops, directive.Op)
	}
	return ops
}

func (s *directiveSink) next(t *testing.T) webview2host.Directive {
	t.Helper()
	select {
	case directive := <-s.feed:
		return directive
	case <-time.After(hostedTestDeadline):
		t.Fatal("timed out waiting for a directive")
	}
	return webview2host.Directive{}
}

// expectOps asserts the exact directive sequence, which is the whole
// observable contract of a failed create: ask, then clean up.
func (s *directiveSink) expectOps(t *testing.T, want ...webview2host.Op) {
	t.Helper()
	got := s.ops()
	if len(got) != len(want) {
		t.Fatalf("directives %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("directives %v, want %v", got, want)
		}
	}
}

type stubRelay struct {
	url string
	err error
}

func (r stubRelay) BrowserWebSocketURL(context.Context) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return r.url, nil
}

func newTestHostedEngine(t *testing.T, relay hostRelay, events engineEvents) (*hostedEngine, *directiveSink) {
	t.Helper()
	sink := newDirectiveSink()
	engine := newHostedEngine(relay, sink.send, events)
	engine.logf = func(format string, args ...any) { t.Logf(format, args...) }
	engine.createTimeout = 150 * time.Millisecond
	engine.attachTimeout = 150 * time.Millisecond
	engine.clearTimeout = 150 * time.Millisecond
	return engine, sink
}

func testProfile(t *testing.T, engine *hostedEngine) *hostedProfile {
	t.Helper()
	profile, err := engine.NewProfile(context.Background(), profileOptions{Workspace: "/home/dev/repo"})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	return profile.(*hostedProfile)
}

func TestHostedProfileIDIsDerivedStableAndValid(t *testing.T) {
	first, err := hostedProfileID("/home/dev/repo")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	again, err := hostedProfileID("/home/dev/repo")
	if err != nil {
		t.Fatalf("derive again: %v", err)
	}
	if first != again {
		t.Fatalf("profile id is not stable: %q vs %q", first, again)
	}
	other, err := hostedProfileID("/home/dev/other")
	if err != nil {
		t.Fatalf("derive other: %v", err)
	}
	if other == first {
		t.Fatal("two workspaces share one profile id")
	}
	if err := webview2host.ValidateProfileID(first); err != nil {
		t.Fatalf("derived id is not a legal profile id: %v", err)
	}
	if _, err := hostedProfileID("   "); err == nil {
		t.Fatal("an empty workspace was accepted")
	}
}

func TestHostedEngineStartRequiresWiring(t *testing.T) {
	engine := newHostedEngine(nil, nil, engineEvents{})
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

func TestHostedEngineCreateTimesOutAndClosesTheController(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("unused")}, engineEvents{})
	profile := testProfile(t, engine)

	started := time.Now()
	_, err := profile.NewPage(context.Background(), pageHooks{})
	if err == nil {
		t.Fatal("a create with no answer succeeded")
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("error %v does not name the timeout", err)
	}
	if elapsed := time.Since(started); elapsed > hostedTestDeadline {
		t.Fatalf("the create took %s: the wait is not bounded", elapsed)
	}
	// The launcher may be mid-create, so the controller is closed anyway.
	sink.expectOps(t, webview2host.OpCreate, webview2host.OpClose)
}

func TestHostedEngineCreateRespectsCallerCancellation(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("unused")}, engineEvents{})
	engine.createTimeout = time.Hour
	profile := testProfile(t, engine)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := profile.NewPage(ctx, pageHooks{})
		done <- err
	}()
	sink.next(t)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("a cancelled create did not return")
	}
	sink.expectOps(t, webview2host.OpCreate, webview2host.OpClose)
}

// answer replies to the next create directive with one report, which is
// what a launcher does over the notification bridge.
func answerCreate(t *testing.T, engine *hostedEngine, sink *directiveSink, kind webview2host.ReportKind, detail string) {
	t.Helper()
	go func() {
		directive := sink.next(t)
		if directive.Op != webview2host.OpCreate {
			t.Errorf("first directive was %q, want create", directive.Op)
			return
		}
		engine.Report(directive.PageID, kind, detail)
	}()
}

func TestHostedEngineSurfacesACreateFailedReport(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("unused")}, engineEvents{})
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreateFailed, "WebView2 runtime missing")

	_, err := profile.NewPage(context.Background(), pageHooks{})
	if err == nil {
		t.Fatal("a failed create succeeded")
	}
	if !strings.Contains(err.Error(), "WebView2 runtime missing") {
		t.Fatalf("error %v drops the launcher's reason", err)
	}
	// Nothing was created, so nothing is closed.
	sink.expectOps(t, webview2host.OpCreate)
}

func TestHostedEngineSurfacesACloseRacingTheCreate(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("unused")}, engineEvents{})
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportClosed, "window closed")

	_, err := profile.NewPage(context.Background(), pageHooks{})
	if err == nil {
		t.Fatal("a create closed under us succeeded")
	}
	if !strings.Contains(err.Error(), string(webview2host.ReportClosed)) {
		t.Fatalf("error %v does not name the report", err)
	}
}

func TestHostedEngineRefusesACreatedReportWithNoTarget(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("unused")}, engineEvents{})
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreated, "   ")

	_, err := profile.NewPage(context.Background(), pageHooks{})
	if err == nil {
		t.Fatal("a created report with no CDP target was accepted")
	}
	if !strings.Contains(err.Error(), "no CDP target") {
		t.Fatalf("error %v does not name the missing target", err)
	}
	sink.expectOps(t, webview2host.OpCreate, webview2host.OpClose)
}

func TestHostedEngineClosesTheControllerWhenTheAttachFails(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("no browser host tunnel is connected")}, engineEvents{})
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-1")

	_, err := profile.NewPage(context.Background(), pageHooks{})
	if err == nil {
		t.Fatal("a create with an unreachable CDP endpoint succeeded")
	}
	if !strings.Contains(err.Error(), "reach the pane CDP endpoint") {
		t.Fatalf("error %v does not name the relay failure", err)
	}
	// A real controller exists on the far side; leaving it would be a
	// window the user cannot close.
	sink.expectOps(t, webview2host.OpCreate, webview2host.OpClose)
}

func TestHostedEngineClosesAnOrphanCreatedReport(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("unused")}, engineEvents{})
	engine.Report("0123456789abcdef0123456789abcdef", webview2host.ReportCreated, "TARGET-9")
	sink.expectOps(t, webview2host.OpClose)
	if got := sink.next(t); got.PageID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("closed %q", got.PageID)
	}
}

func TestHostedEngineInterruptReleasesAPendingCreate(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("unused")}, engineEvents{})
	engine.createTimeout = time.Hour
	profile := testProfile(t, engine)

	done := make(chan error, 1)
	go func() {
		_, err := profile.NewPage(context.Background(), pageHooks{})
		done <- err
	}()
	sink.next(t)
	engine.Interrupt()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("got %v, want an interruption", err)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("Interrupt did not release the pending create")
	}
}

func TestHostedEngineRetiresAPageOnTheLauncherReports(t *testing.T) {
	for _, kind := range []webview2host.ReportKind{webview2host.ReportClosed, webview2host.ReportProcessFailed} {
		t.Run(string(kind), func(t *testing.T) {
			var closed []string
			engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{
				PageClosed: func(handle string) { closed = append(closed, handle) },
			})
			engine.bind("page1", "TARGET-1")

			engine.Report("page1", kind, "")
			if len(closed) != 1 || closed[0] != "page1" {
				t.Fatalf("PageClosed saw %v", closed)
			}
			// The retirement is the engine's whole bookkeeping, so a repeat
			// report must not reach the Manager twice.
			engine.Report("page1", kind, "")
			if len(closed) != 1 {
				t.Fatalf("a repeated report was reported again: %v", closed)
			}
		})
	}
}

func TestHostedEngineShowAndHideAreDedupedAndPageScoped(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	engine.bind("page1", "TARGET-1")

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
	engine.bind("page1", "TARGET-1")
	rect := PaneRect{X: 12, Y: 34, Width: 800, Height: 600, ViewportWidth: 1600, ViewportHeight: 900, Visible: true}
	engine.SetPageBounds("page1", rect)
	engine.OpenPageDevTools("page1")
	sink.expectOps(t, webview2host.OpBounds, webview2host.OpDevTools)

	bounds := sink.next(t)
	if bounds.X != 12 || bounds.Y != 34 || bounds.W != 800 || bounds.H != 600 || bounds.VW != 1600 || bounds.VH != 900 {
		t.Fatalf("bounds directive lost its rectangle: %+v", bounds)
	}
	// The presentation sync re-sends the active rect on every selection and
	// page-list change; an unmoved rect costs no directive.
	engine.SetPageBounds("page1", rect)
	sink.expectOps(t, webview2host.OpBounds, webview2host.OpDevTools)
	// A page this engine does not own is not a directive, same as show/hide.
	engine.SetPageBounds("page2", PaneRect{X: 1, Y: 2, Width: 300, Height: 400})
	sink.expectOps(t, webview2host.OpBounds, webview2host.OpDevTools)
	// A rectangle the launcher would refuse never reaches the wire.
	engine.SetPageBounds("page1", PaneRect{})
	sink.expectOps(t, webview2host.OpBounds, webview2host.OpDevTools)
}

func TestHostedEngineRefusesAnInvalidPageID(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	engine.bind("not a page id", "TARGET-1")
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
		PageClosed:      func(handle string) { closed <- handle },
		PageInfoChanged: func(handle, url, title string) { infos = append(infos, handle+" "+url+" "+title) },
	})
	engine.bind("page1", "TARGET-1")

	engine.dispatchEvent(&target.EventTargetInfoChanged{TargetInfo: &target.Info{
		TargetID: "TARGET-1", Type: "page", URL: "https://example.test/", Title: "Example",
	}})
	if len(infos) != 1 || infos[0] != "page1 https://example.test/ Example" {
		t.Fatalf("PageInfoChanged saw %v", infos)
	}
	// A target this engine never created is dropped rather than reported
	// under a handle the Manager does not own.
	engine.dispatchEvent(&target.EventTargetInfoChanged{TargetInfo: &target.Info{
		TargetID: "TARGET-9", Type: "page", URL: "https://elsewhere.test/",
	}})
	if len(infos) != 1 {
		t.Fatalf("an unknown target was reported: %v", infos)
	}

	engine.dispatchEvent(&target.EventTargetDestroyed{TargetID: "TARGET-1"})
	select {
	case handle := <-closed:
		if handle != "page1" {
			t.Fatalf("PageClosed saw %q", handle)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("targetDestroyed did not retire the page")
	}
}

func TestHostedProfileDisposeClosesTheWholeProfile(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	profile, err := engine.NewProfile(context.Background(), profileOptions{Workspace: "/home/dev/repo", Persist: false})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	if err := profile.Dispose(context.Background()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	sink.expectOps(t, webview2host.OpCloseProfile)
	directive := sink.next(t)
	if directive.ProfileID != profile.Handle() || !directive.Ephemeral {
		t.Fatalf("close-profile directive is %+v", directive)
	}
}

func TestHostedProfileReportsNoPopups(t *testing.T) {
	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	profile := testProfile(t, engine)

	if _, err := profile.AttachPage(context.Background(), "TARGET-1", pageHooks{}); err == nil {
		t.Fatal("the pane host adopted a popup it can never report")
	}
}

func TestHostedEngineDiscardPageClosesTheController(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	engine.DiscardPage("0123456789abcdef0123456789abcdef")
	sink.expectOps(t, webview2host.OpClose)
}

// ---------------------------------------------------------------------
// Clear site data
// ---------------------------------------------------------------------

// On the Windows/WSL deployment the Manager's own profile tree is empty and
// every cookie jar is on the far side of the WSL boundary, so this seam is
// the ENTIRE clear. An engine that stopped implementing it would make the
// Settings button a silent no-op again, with nothing failing to compile.
var _ engineSiteData = (*hostedEngine)(nil)

// answerClear replies to the next clear-data directive, which is what the
// launcher does once it has closed every controller, released the
// environment, and finished deleting its user-data folder.
// It hands back the directive it answered, because the feed is consumed by
// the reply and a caller cannot read it twice.
func answerClear(t *testing.T, engine *hostedEngine, sink *directiveSink, kind webview2host.ReportKind, detail string) <-chan webview2host.Directive {
	t.Helper()
	seen := make(chan webview2host.Directive, 1)
	go func() {
		directive := sink.next(t)
		if directive.Op != webview2host.OpClearData {
			t.Errorf("first directive was %q, want clear-data", directive.Op)
			return
		}
		seen <- directive
		engine.Report(directive.PageID, kind, detail)
	}()
	return seen
}

func TestHostedEngineClearSiteDataDispatchesAndSucceeds(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	seen := answerClear(t, engine, sink, webview2host.ReportCleared, "")

	if err := engine.ClearSiteData(context.Background()); err != nil {
		t.Fatalf("clear site data: %v", err)
	}
	sink.expectOps(t, webview2host.OpClearData)
	// The clear addresses the whole user-data folder: naming a profile would
	// be a claim the launcher cannot honour, since one folder holds every
	// workspace's named profile.
	directive := <-seen
	if directive.ProfileID != "" {
		t.Fatalf("clear-data named profile %q", directive.ProfileID)
	}
	if err := webview2host.ValidatePageID(directive.PageID); err != nil {
		t.Fatalf("clear-data correlation id is not addressable: %v", err)
	}
}

func TestHostedEngineClearSiteDataSurfacesTheLauncherFailure(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	answerClear(t, engine, sink, webview2host.ReportClearFailed, "delete C:\\...\\browser-profiles: Access is denied.")

	err := engine.ClearSiteData(context.Background())
	if err == nil {
		t.Fatal("a failed clear reported success; the user would believe cookies were destroyed")
	}
	if !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("error %v drops the launcher's reason", err)
	}
}

func TestHostedEngineClearSiteDataIsBounded(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})

	started := time.Now()
	err := engine.ClearSiteData(context.Background())
	if err == nil {
		t.Fatal("a clear with no answer succeeded")
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("error %v does not name the timeout", err)
	}
	if elapsed := time.Since(started); elapsed > hostedTestDeadline {
		t.Fatalf("the clear took %s: the wait is not bounded", elapsed)
	}
	// Nothing is closed on the way out: the clear owns no controller, and
	// the launcher closes its own before deleting anything.
	sink.expectOps(t, webview2host.OpClearData)
}

func TestHostedEngineClearSiteDataRespectsCancellationAndInterrupt(t *testing.T) {
	t.Run("caller cancellation", func(t *testing.T) {
		engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
		engine.clearTimeout = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- engine.ClearSiteData(ctx) }()
		sink.next(t)
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want context.Canceled", err)
			}
		case <-time.After(hostedTestDeadline):
			t.Fatal("a cancelled clear did not return")
		}
	})

	t.Run("shutdown interrupt", func(t *testing.T) {
		engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
		engine.clearTimeout = time.Hour
		done := make(chan error, 1)
		go func() { done <- engine.ClearSiteData(context.Background()) }()
		sink.next(t)
		engine.Interrupt()

		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "interrupted") {
				t.Fatalf("got %v, want an interruption", err)
			}
		case <-time.After(hostedTestDeadline):
			t.Fatal("Interrupt did not release the pending clear")
		}
	})
}

// A late or duplicated clear report names a correlation id, not a page. It
// must not be mistaken for one: closing or retiring "page" clear-abc123
// would tear down nothing and tell the Manager a page it never knew about
// had died.
func TestHostedEngineIgnoresAnAbandonedClearReport(t *testing.T) {
	var closed []string
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{
		PageClosed: func(handle string) { closed = append(closed, handle) },
	})
	engine.Report("0123456789abcdef0123456789abcdef", webview2host.ReportCleared, "")
	engine.Report("0123456789abcdef0123456789abcdef", webview2host.ReportClearFailed, "too late")
	sink.expectOps(t)
	if len(closed) != 0 {
		t.Fatalf("a clear report was mistaken for a page: %v", closed)
	}
}

// The Manager is the only router of launcher reports, and it refuses any
// kind it does not recognise. A clear whose report kind never reached
// ValidKind would be rejected at the RPC and the engine would wait out its
// whole timeout for an answer that was already in the building.
func TestManagerRoutesClearReportsToTheHostedEngine(t *testing.T) {
	sink := newDirectiveSink()
	manager := NewManager(t.TempDir(), Config{}, ManagerOptions{PaneHost: &PaneHostOptions{
		Directive: sink.send,
		Relay:     stubRelay{},
	}})
	engine, ok := manager.engine.(*hostedEngine)
	if !ok {
		t.Fatalf("engine is %T, want the hosted engine", manager.engine)
	}
	engine.clearTimeout = time.Hour

	done := make(chan error, 1)
	go func() { done <- engine.ClearSiteData(context.Background()) }()
	directive := sink.next(t)
	if err := manager.ReportPaneHost(directive.PageID, webview2host.ReportCleared, ""); err != nil {
		t.Fatalf("route a cleared report: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clear site data: %v", err)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("the routed report never reached the clear's waiter")
	}
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

// ---------------------------------------------------------------------
// The browser-level dial
// ---------------------------------------------------------------------

// fakeCDPBrowser is a loopback websocket endpoint speaking just enough CDP
// to accept a browser-level connection: every command is recorded and
// answered with an empty success. It is what the launcher's WebView2
// debugging endpoint looks like to the dial — a browser that answers
// commands but has NO tabs of its own to hand out.
func fakeCDPBrowser(t *testing.T) (wsURL string, methods func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			payload, err := wsutil.ReadClientText(conn)
			if err != nil {
				return
			}
			var msg struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(payload, &msg); err != nil {
				t.Errorf("bad CDP frame %q: %v", payload, err)
				return
			}
			mu.Lock()
			seen = append(seen, msg.Method)
			mu.Unlock()
			reply, _ := json.Marshal(map[string]any{"id": msg.ID, "result": map[string]any{}})
			if err := wsutil.WriteServerText(conn, reply); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// The regression this pins: chromedp.Run on a target-less remote context
// issues Target.createTarget, which Chrome quietly answers with a
// throwaway tab and WebView2 refuses with `-32000 no browser is open` —
// a WebView2 target exists only as a launcher-created controller, so the
// browser-level dial must never create one (2026-08-31, the first live
// pane attach). It must instead enable target discovery, which is the
// only thing that feeds dispatchEvent's targetDestroyed backstop.
func TestHostedEngineBrowserDialCreatesNoTargetAndEnablesDiscovery(t *testing.T) {
	wsURL, methods := fakeCDPBrowser(t)
	engine, _ := newTestHostedEngine(t, stubRelay{url: wsURL}, engineEvents{})
	engine.attachTimeout = 5 * time.Second

	if _, err := engine.ensureBrowser(); err != nil {
		t.Fatalf("ensureBrowser against the fake endpoint: %v", err)
	}
	defer engine.Interrupt()

	sawDiscovery := false
	for _, method := range methods() {
		if method == "Target.createTarget" {
			t.Fatal("the browser-level dial created a target; WebView2 answers that with -32000 no browser is open")
		}
		if method == "Target.setDiscoverTargets" {
			sawDiscovery = true
		}
	}
	if !sawDiscovery {
		t.Fatal("the dial never enabled target discovery; the targetDestroyed backstop would hear nothing")
	}
}

// A file the agent opens is on the WSL filesystem, but the renderer that
// must load it is a Windows-side WebView2, so the URL has to carry the
// Windows VIEW of the path (2026-08-31: file:///home/... navigated a live
// pane to ERR_FILE_NOT_FOUND). The engine advertises that through
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
// the authority check blocks the navigation OpenFile just authorized
// (live incident 2026-08-31, ERR_BLOCKED_BY_CLIENT).
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
