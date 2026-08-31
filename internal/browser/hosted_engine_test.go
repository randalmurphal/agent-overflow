package browser

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
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
	_, err := profile.NewPage(context.Background(), pageHooks{}, nil)
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
		_, err := profile.NewPage(ctx, pageHooks{}, nil)
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

	_, err := profile.NewPage(context.Background(), pageHooks{}, nil)
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

	_, err := profile.NewPage(context.Background(), pageHooks{}, nil)
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

	_, err := profile.NewPage(context.Background(), pageHooks{}, nil)
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

	_, err := profile.NewPage(context.Background(), pageHooks{}, nil)
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
		_, err := profile.NewPage(context.Background(), pageHooks{}, nil)
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
	engine.SetPageBounds("page1", 12, 34, 800, 600)
	engine.OpenPageDevTools("page1")
	sink.expectOps(t, webview2host.OpBounds, webview2host.OpDevTools)

	bounds := sink.next(t)
	if bounds.X != 12 || bounds.Y != 34 || bounds.W != 800 || bounds.H != 600 {
		t.Fatalf("bounds directive lost its rectangle: %+v", bounds)
	}
	// A rectangle the launcher would refuse never reaches the wire.
	engine.SetPageBounds("page1", 0, 0, 0, 0)
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
	profile, err := engine.NewProfile(context.Background(), profileOptions{Workspace: "/home/dev/repo", Ephemeral: true})
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

func TestHostedProfileHasNoCookieCheckpointAndNoPopups(t *testing.T) {
	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	profile := testProfile(t, engine)

	cookies, err := profile.Cookies(context.Background())
	if err != nil || cookies != nil {
		t.Fatalf("Cookies returned (%v, %v), want (nil, nil)", cookies, err)
	}
	if _, err := profile.AttachPage(context.Background(), "TARGET-1", pageHooks{}, nil); err == nil {
		t.Fatal("the pane host adopted a popup it can never report")
	}
}

func TestHostedEngineDiscardPageClosesTheController(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	engine.DiscardPage("0123456789abcdef0123456789abcdef")
	sink.expectOps(t, webview2host.OpClose)
}

// The pane presentation path the companion drives is an engine
// capability, not a Manager branch: only the hosted engine answers it.
var _ paneHost = (*hostedEngine)(nil)

func TestManagerSelectsTheHostedEngineFromThePaneHostOptions(t *testing.T) {
	sink := newDirectiveSink()
	manager := NewManager(nil, t.TempDir(), Config{}, ManagerOptions{PaneHost: &PaneHostOptions{
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

func TestManagerWithoutAPaneHostKeepsManagedChrome(t *testing.T) {
	manager := NewManager(nil, t.TempDir(), Config{}, ManagerOptions{})
	if _, ok := manager.engine.(*cdpEngine); !ok {
		t.Fatalf("engine is %T, want managed Chrome", manager.engine)
	}
	if err := manager.ReportPaneHost("page1", webview2host.ReportClosed, ""); err == nil {
		t.Fatal("a deployment with no pane host accepted a report")
	}
}
