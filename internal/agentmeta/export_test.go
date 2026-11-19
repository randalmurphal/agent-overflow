package agentmeta

import "testing"

// TestReportKind is a report kind only this package's tests define, as a
// kind added in Go alone would be: no reader outside Go names it.
const TestReportKind ReportKind = "test-only"

// WithTestReportKind defines TestReportKind for the rest of t. A test that
// calls it does not run in parallel: the kinds are the package's.
func WithTestReportKind(t *testing.T) {
	t.Helper()
	reportKinds[TestReportKind] = reportKind{
		category: "test-only", label: "Probe", title: "Test-only report", badge: "Probe", data: true,
	}
	t.Cleanup(func() { delete(reportKinds, TestReportKind) })
}
