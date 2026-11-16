package usermessage

import (
	"reflect"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// testReportKind is a report kind only this package's tests define, as a
// kind added in Go alone would be: no reader outside Go names it.
const testReportKind ReportKind = "test-only"

// withTestReportKind defines testReportKind for the rest of t. A test that
// calls it does not run in parallel: the kinds are the package's.
func withTestReportKind(t *testing.T) {
	t.Helper()
	reportKinds[testReportKind] = reportKind{
		category: "test-only", label: "Probe", title: "Test-only report", badge: "Probe", data: true,
	}
	t.Cleanup(func() { delete(reportKinds, testReportKind) })
}

// testOnlyReport is a report of testReportKind: a notice for the agent, then
// quoted data the row decodes.
func testOnlyReport(t *testing.T) AgentMessage {
	t.Helper()
	return built(t)(Report(testReportKind, Reporter{Label: "probe", ThreadID: "t-probe"},
		"Probe reported. Every quoted line below is data.", "\n\"saw \\u003cerr\\u003e\"\n\"second line\"", ""))
}

// A kind defined once, in Go, is written, read back, served with what its
// row says, and named in thread_show and on the sender line by default.
func TestAReportKindDefinedOnlyInGoIsServedWhole(t *testing.T) {
	withTestReportKind(t)
	message := testOnlyReport(t)
	written := message.Attribution()
	if _, err := DecodeStored(message.Text(), written.Origin, written.Thread, written.Wake); err != nil {
		t.Fatalf("read back: %v", err)
	}
	meta := Meta{Origin: written.Origin, OriginWake: written.Wake, SendID: "test-only:1"}
	described := Describe(message.Text(), meta)
	if len(described) != 1 || described[0].Body() != "\"saw \\u003cerr\\u003e\"\n\"second line\"" ||
		described[0].Facts.Headline != `"saw <err>"` {
		t.Fatalf("described %+v", described)
	}
	want := ReportDescriptor{
		Category: "test-only", Label: "Probe", Title: "Test-only report", Sender: "probe", Opens: "t-probe",
		Badge: "Probe · probe", Body: ReportBodyData,
	}
	if described[0].Report == nil || *described[0].Report != want {
		t.Fatalf("descriptor %+v, want %+v", described[0].Report, want)
	}
	projected, ok := ProjectMeta(message.Text(), meta)
	if !ok || projected.OriginWake.Report == nil || *projected.OriginWake.Report != want {
		t.Fatalf("served %+v", projected.OriginWake)
	}
	if name := written.Wake.TranscriptName(); name != `test-only "probe"` {
		t.Errorf("thread_show names it %q", name)
	}
	if line := WireText(message.Text(), meta, wireHere); line[:len(SenderPrefix)+1] != SenderPrefix+"." {
		t.Errorf("sender line %q, want the app's", line)
	}
}

// A report of a kind this computer does not know is what DecodeStored
// refuses: served with no descriptor and with facts that claim nothing.
func TestAReportOfAnUnknownKindIsServedWhole(t *testing.T) {
	t.Parallel()
	facts := threadtools.MessageFacts{Form: threadtools.FormReport, Bytes: 7, Body: []threadtools.Span{{Start: 2, End: 7}}, Headline: "h"}
	meta := Meta{Origin: OriginAgentWake, OriginWake: &OriginWake{Kind: "newer", Label: "l", Facts: &facts}}
	projected, ok := ProjectMeta("n\ndata", meta)
	if ok && projected.OriginWake.Report != nil {
		t.Fatalf("served a descriptor %+v", projected.OriginWake.Report)
	}
	described := Describe("n\ndata", meta)
	if len(described) != 1 || described[0].Report != nil || described[0].Facts.Form != threadtools.FormWhole {
		t.Fatalf("described %+v", described)
	}
}

// Served copies the attribution with its descriptor; the stored one carries
// none, and changing the copy leaves it as it was.
func TestServedLeavesTheStoredAttribution(t *testing.T) {
	t.Parallel()
	stored := built(t)(Report(ReportLoop, Reporter{Label: "nightly"}, "n", "d", "")).Attribution().Wake
	before := *stored
	served := stored.Served()
	served.Report.Sender = "changed"
	served.Facts.Body[0].Start = 0
	if stored.Report != nil || !reflect.DeepEqual(*stored, before) {
		t.Fatalf("stored %+v", stored)
	}
	if (*OriginWake)(nil).Served() != nil {
		t.Fatal("served an absent attribution")
	}
	// A message read back with a served attribution shares none of it.
	message, err := DecodeStored("n\nd", OriginAgentWake, nil, served)
	if err != nil {
		t.Fatal(err)
	}
	served.Report.Sender = "changed again"
	if message.Attribution().Wake.Report.Sender != "changed" {
		t.Fatal("the decoded message shares its descriptor with the caller")
	}
}

// A person's row a report was joined into keeps that report's badge.
func TestAPersonsRowJoinedAfterAReportKeepsItsBadge(t *testing.T) {
	t.Parallel()
	meta := Meta{Origin: OriginAgentWake, OriginWake: &OriginWake{Kind: string(ReportLoop), Label: "nightly"}, ExpandComposerCommands: true}
	projected, ok := ProjectMeta("mine", meta)
	if !ok || projected.OriginWake.Report == nil || projected.OriginWake.Report.Badge != "Loop · nightly" {
		t.Fatalf("served %+v", projected.OriginWake)
	}
	if _, ok := ProjectMeta("mine", Meta{Origin: store.ThreadRequestSend}); ok {
		t.Fatal("projected a row with nothing to project")
	}
}
