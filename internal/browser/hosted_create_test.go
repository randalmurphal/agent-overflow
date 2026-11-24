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

// A create the launcher answered with anything but a usable page fails
// with an error naming why, and closes the controller exactly when one may
// exist on the far side.
func TestHostedEngineCreateFailureNamesItsCause(t *testing.T) {
	for _, tc := range []struct {
		name     string
		relayErr string
		kind     webview2host.ReportKind
		detail   string
		want     string
		ops      []webview2host.Op
	}{
		// Nothing was created, so nothing is closed.
		{"a create-failed report", "unused", webview2host.ReportCreateFailed, "WebView2 runtime missing", "WebView2 runtime missing", []webview2host.Op{webview2host.OpCreate}},
		{"a close racing the create", "unused", webview2host.ReportClosed, "window closed", string(webview2host.ReportClosed), []webview2host.Op{webview2host.OpCreate}},
		{"a created report with no target", "unused", webview2host.ReportCreated, "   ", "no CDP target", []webview2host.Op{webview2host.OpCreate, webview2host.OpClose}},
		// A real controller exists on the far side; leaving it would be a
		// window the user cannot close.
		{"an attach that fails", "no browser host tunnel is connected", webview2host.ReportCreated, "TARGET-1", "reach the pane CDP endpoint", []webview2host.Op{webview2host.OpCreate, webview2host.OpClose}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New(tc.relayErr)}, engineEvents{})
			profile := testProfile(t, engine)
			answerCreate(t, engine, sink, tc.kind, tc.detail)
			_, err := profile.NewPage(context.Background(), pageHooks{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewPage = %v, want an error naming %q", err, tc.want)
			}
			sink.expectOps(t, tc.ops...)
		})
	}
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
