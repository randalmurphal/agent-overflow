package webview2host

import "testing"

// A DevTools report round-trips through its detail, and one naming no
// request is refused.
func TestADevToolsReportRoundTrips(t *testing.T) {
	for _, report := range []DevToolsReport{{Request: 1}, {Request: 7, Error: "E_FAIL"}} {
		if got, err := ParseDevToolsReport(report.Encode()); err != nil || got != report {
			t.Fatalf("round trip of %+v answered %+v, %v", report, got, err)
		}
	}
	for _, detail := range []string{`{"error":"E_FAIL"}`, `{"request":0}`, `not json`} {
		if _, err := ParseDevToolsReport(detail); err == nil {
			t.Fatalf("ParseDevToolsReport(%q) succeeded", detail)
		}
	}
	if ReportDevToolsOpened != "devtools-opened" || ReportDevToolsFailed != "devtools-failed" {
		t.Fatalf("the DevTools report kinds are %q / %q", ReportDevToolsOpened, ReportDevToolsFailed)
	}
}
