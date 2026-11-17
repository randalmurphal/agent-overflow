//go:build windows

package webview2host

import (
	"testing"
)

// A popup whose report could not be delivered is closed, with its popups
// not announced yet, so no hidden controller is left that the backend will
// never adopt; against stand-in WebView2 objects and a stand-in report
// sink.
func TestAPopupWhoseReportWasNotDeliveredCloses(t *testing.T) {
	cases := []struct {
		name string
		// fail answers the sink that loses the popup's report.
		fail func(r *popupRig) func(rigReport)
	}{
		{"the call fails", func(r *popupRig) func(rigReport) {
			return func(report rigReport) {
				if report.kind == ReportPopup {
					go r.host.ReportUndelivered(report.page, report.kind)
				}
			}
		}},
		{"the call times out after it landed", func(r *popupRig) func(rigReport) {
			return func(report rigReport) {
				if report.kind == ReportPopup {
					// The report stays recorded: the backend has it.
					go r.host.ReportUndelivered(report.page, report.kind)
				}
			}
		}},
		{"the bridge is down at the announcement", func(r *popupRig) func(rigReport) {
			return func(report rigReport) {
				r.host.ReportUndelivered(report.page, report.kind)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPopupRig(t)
			_, opener := r.createPage("opener", "prof", false)
			popup, controller := r.openPopup(opener, newFakeWindowArgs(popupURL))
			popup.popup.held.findUnavailable = false
			child, childController := r.openPopup(controller, newFakeWindowArgs(popupURL))
			child.popup.held.findUnavailable = false
			r.mu.Lock()
			r.sink = tc.fail(r)
			r.mu.Unlock()
			answerTarget(t, controller, "T-popup")
			sameReports(t, "a popup whose report was lost", kinds(t, r.awaitReport(popup.id, ReportClosed)),
				popup.id+":popup(opener,T-popup)", popup.id+":closed")
			if r.page(popup.id) != nil || r.page(child.id) != nil {
				t.Fatal("a popup whose report was lost, or its popup, is still a page")
			}
			closed(t, controller)
			closed(t, childController)
			if got := r.awaitReports(0); len(got) != 0 {
				t.Fatalf("after the popup closed: %v", kinds(t, got))
			}
			r.mu.Lock()
			r.sink = nil
			r.mu.Unlock()
			r.done()
			closed(t, opener)
		})
	}
}

// A report other than a popup's that was not delivered closes nothing: the
// backend re-derives it.
func TestAnUndeliveredReportOfAnotherKindClosesNothing(t *testing.T) {
	r := newPopupRig(t)
	page, pageController := r.createPage("page", "prof", false)
	popup, controller := r.openPopup(pageController, newFakeWindowArgs(popupURL))
	answerTarget(t, controller, "T-popup")
	r.takeReports()
	for _, kind := range []ReportKind{ReportCreated, ReportDownload, ReportClosed, ReportProcessFailed} {
		r.host.ReportUndelivered(popup.id, kind)
		r.host.ReportUndelivered(page.id, kind)
	}
	r.host.ReportUndelivered(page.id, ReportPopup)
	r.pump()
	if r.page(page.id) == nil || r.page(popup.id) == nil {
		t.Fatal("an undelivered report closed a page")
	}
	r.done()
}
