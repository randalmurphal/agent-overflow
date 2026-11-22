package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

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
	if err := webview2host.ValidateProfileID(string(first)); err != nil {
		t.Fatalf("derived id is not a legal profile id: %v", err)
	}
	if _, err := hostedProfileID("   "); err == nil {
		t.Fatal("an empty workspace was accepted")
	}
}

func TestHostedProfileDisposeClosesTheWholeProfile(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	profile, err := engine.NewProfile(context.Background(), profileOptions{Jar: "/home/dev/repo", Ephemeral: true})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	if err := profile.Dispose(context.Background()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	sink.expectOps(t, webview2host.OpCloseProfile)
	directive := sink.next(t)
	if profileHandle(directive.ProfileID) != profile.Handle() {
		t.Fatalf("close-profile directive is %+v", directive)
	}
}

// A profile attaches only a popup the launcher reported in it, and not
// once the popup closed (hosted_popups_test.go covers the adoption).
func TestHostedProfileAttachesOnlyItsReportedPopups(t *testing.T) {
	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{PageClosed: func(ids.PageHandle) {}})
	profile := testProfile(t, engine)
	engine.mu.Lock()
	engine.popups["popup-other"] = &hostedPopup{target: "T-other", profile: "another-profile"}
	engine.mu.Unlock()
	bindTestPage(t, engine, "opener", "T-opener")
	engine.mu.Lock()
	engine.pageProfile["opener"] = profile.id
	engine.mu.Unlock()
	pendingTestPopup(engine, "popup-closed", "T-closed", "opener")
	engine.Report("popup-closed", webview2host.ReportClosed, "")
	settleReports(t, engine, "")

	for handle, why := range map[string]string{
		"TARGET-1":     "reported no popup",
		"popup-other":  "reported no popup",
		"popup-closed": "reported no popup",
	} {
		if _, err := profile.AttachPage(context.Background(), ids.PageHandle(handle), pageHooks{}); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("attach %s: %v, want %q", handle, err, why)
		}
	}
}

// ---------------------------------------------------------------------
// Clear site data
// ---------------------------------------------------------------------

// On the Windows/WSL deployment the Manager's own profile tree is empty and
// every cookie jar is on the far side of the WSL boundary, so this seam is
// the ENTIRE clear. An engine that stopped implementing it would make the
// Settings button a silent no-op again, with nothing failing to compile.
var _ engineSiteData = (*hostedEngine)(nil)

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
		PageClosed: func(handle ids.PageHandle) { closed = append(closed, string(handle)) },
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
