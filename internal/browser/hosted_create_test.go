package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
)

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

// A created report naming a target another page has, bound or a popup's,
// is refused before the attach, whose end would close that page's target,
// and the target stays its page's.
func TestHostedEngineRefusesACreatedReportNamingAnotherPagesTarget(t *testing.T) {
	for _, tc := range []struct{ target, owner string }{{"TARGET-1", "page1"}, {"TARGET-2", "popup"}} {
		engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("no browser host tunnel is connected")}, engineEvents{})
		profile := testProfile(t, engine)
		bindTestPage(t, engine, "page1", "TARGET-1")
		pendingTestPopup(engine, "popup", "TARGET-2", "page1")
		answerCreate(t, engine, sink, webview2host.ReportCreated, tc.target)
		_, err := profile.NewPage(context.Background(), pageHooks{})
		if err == nil || !strings.Contains(err.Error(), "which page "+tc.owner+" has") {
			t.Fatalf("a create naming %s's target: %v, want it refused", tc.owner, err)
		}
		sink.expectOps(t, webview2host.OpCreate, webview2host.OpClose)
		if owner, _ := engine.pageForTarget(target.ID(tc.target)); owner != ids.PageHandle(tc.owner) {
			t.Fatalf("target %s is page %q's after the refusal, want %s's", tc.target, owner, tc.owner)
		}
	}
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
