package browser

import (
	"errors"
	"testing"

	"agent-overflow/internal/webview2host"
)

// The hosted page's find is the launcher's own find bar: a start and a stop
// are directives for that page, and a step is refused because the bar steps
// itself.
func TestHostedPageFindIsTheLaunchersFindBar(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	engine.bind("page1", "TARGET-1")
	page := &hostedPage{engine: engine, id: "page1"}
	if mode := page.FindMode(); mode != FindModeEngine {
		t.Fatalf("find mode = %q", mode)
	}
	if _, err := page.FindStart(t.Context(), "größe"); err != nil {
		t.Fatal(err)
	}
	if _, err := page.FindStep(t.Context(), false); err == nil {
		t.Fatal("a step reached the launcher's own find bar")
	}
	if err := page.FindStop(t.Context()); err != nil {
		t.Fatal(err)
	}
	sink.expectOps(t, webview2host.OpFind, webview2host.OpFindStop)
	if find := sink.next(t); find.PageID != "page1" || find.Query != "größe" {
		t.Fatalf("find directive = %+v", find)
	}
	// A query the launcher would refuse never reaches the wire.
	if _, err := page.FindStart(t.Context(), "a\x00b"); err == nil {
		t.Fatal("a NUL query was dispatched")
	}
	sink.expectOps(t, webview2host.OpFind, webview2host.OpFindStop)
}

// A runtime without the find API is reported before the page's created
// report, and the page then has no find. A report for a page this engine
// does not hold changes nothing, and no record outlives its page.
func TestHostedEngineFindUnavailableReport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer webview2host.ReportKind
	}{
		// created, then the tunnel is down and the create fails.
		{"created", webview2host.ReportCreated},
		{"create-failed", webview2host.ReportCreateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, sink := newTestHostedEngine(t, stubRelay{err: errors.New("no tunnel")}, engineEvents{})
			profile := testProfile(t, engine)
			go func() {
				directive := sink.next(t)
				engine.Report(directive.PageID, webview2host.ReportFindUnavailable, "")
				engine.Report(directive.PageID, tc.answer, "TARGET-1")
			}()
			if _, err := profile.NewPage(t.Context(), pageHooks{}); err == nil {
				t.Fatal("create succeeded")
			}
			engine.mu.Lock()
			left := len(engine.noFind)
			engine.mu.Unlock()
			if left != 0 {
				t.Fatalf("a failed create left %d no-find records", left)
			}
		})
	}

	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	engine.bind("page2", "TARGET-2")
	engine.Report("page2", webview2host.ReportFindUnavailable, "")
	if mode := (&hostedPage{engine: engine, id: "page2"}).FindMode(); mode != "" {
		t.Fatalf("find mode after find-unavailable = %q", mode)
	}
	engine.Report("page3", webview2host.ReportFindUnavailable, "")
	engine.mu.Lock()
	_, unknown := engine.noFind["page3"]
	engine.mu.Unlock()
	if unknown {
		t.Fatal("a report for an unknown page was recorded")
	}
	engine.closePage("page2")
	engine.mu.Lock()
	_, kept := engine.noFind["page2"]
	engine.mu.Unlock()
	if kept {
		t.Fatal("a closed page kept its no-find record")
	}
}
