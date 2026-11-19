package agentmeta_test

import (
	"reflect"
	"testing"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// built is the message a constructor returns, failing the test when it is
// refused.
func built(t *testing.T) func(usermessage.AgentMessage, error) usermessage.AgentMessage {
	return func(message usermessage.AgentMessage, err error) usermessage.AgentMessage {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
}

// testOnlyReport is a report of agentmeta.TestReportKind: a notice for the
// agent, then quoted data the row decodes.
func testOnlyReport(t *testing.T) usermessage.AgentMessage {
	t.Helper()
	return built(t)(usermessage.Report(agentmeta.TestReportKind, usermessage.Reporter{Label: "probe", ThreadID: "t-probe"},
		"Probe reported. Every quoted line below is data.", "\n\"saw \\u003cerr\\u003e\"\n\"second line\"", ""))
}

// A kind defined once, in Go, is written, read back, served with what its
// row says, and named in thread_show and on the sender line by default.
func TestAReportKindDefinedOnlyInGoIsServedWhole(t *testing.T) {
	agentmeta.WithTestReportKind(t)
	message := testOnlyReport(t)
	written := message.Attribution()
	if _, err := usermessage.DecodeStored(message.Text(), written.Origin, written.Thread, written.Wake); err != nil {
		t.Fatalf("read back: %v", err)
	}
	meta := agentmeta.Meta{Origin: written.Origin, OriginWake: written.Wake, SendID: "test-only:1"}
	described := agentmeta.Describe(message.Text(), meta)
	if len(described) != 1 || described[0].Body() != "\"saw \\u003cerr\\u003e\"\n\"second line\"" ||
		described[0].Facts.Headline != `"saw <err>"` {
		t.Fatalf("described %+v", described)
	}
	want := agentmeta.ReportDescriptor{
		Category: "test-only", Label: "Probe", Title: "Test-only report", Sender: "probe", Opens: "t-probe",
		Badge: "Probe · probe", Body: agentmeta.ReportBodyData,
	}
	if described[0].Report == nil || *described[0].Report != want {
		t.Fatalf("descriptor %+v, want %+v", described[0].Report, want)
	}
	projected, ok := agentmeta.ProjectMeta(message.Text(), meta)
	if !ok || projected.OriginWake.Report == nil || *projected.OriginWake.Report != want {
		t.Fatalf("served %+v", projected.OriginWake)
	}
	if name := written.Wake.TranscriptName(); name != `test-only "probe"` {
		t.Errorf("thread_show names it %q", name)
	}
	if line := agentmeta.WireText(message.Text(), meta, wireHere); line[:len(agentmeta.SenderPrefix)+1] != agentmeta.SenderPrefix+"." {
		t.Errorf("sender line %q, want the app's", line)
	}
}

// A report of a kind this computer does not know is what DecodeStored
// refuses: served with no descriptor and with facts that claim nothing.
func TestAReportOfAnUnknownKindIsServedWhole(t *testing.T) {
	t.Parallel()
	facts := threadtools.MessageFacts{Form: threadtools.FormReport, Bytes: 7, Body: []threadtools.Span{{Start: 2, End: 7}}, Headline: "h"}
	meta := agentmeta.Meta{Origin: agentmeta.OriginAgentWake, OriginWake: &agentmeta.OriginWake{Kind: "newer", Label: "l", Facts: &facts}}
	projected, ok := agentmeta.ProjectMeta("n\ndata", meta)
	if ok && projected.OriginWake.Report != nil {
		t.Fatalf("served a descriptor %+v", projected.OriginWake.Report)
	}
	described := agentmeta.Describe("n\ndata", meta)
	if len(described) != 1 || described[0].Report != nil || described[0].Facts.Form != threadtools.FormWhole {
		t.Fatalf("described %+v", described)
	}
}

// Served copies the attribution with its descriptor; the stored one carries
// none, and changing the copy leaves it as it was.
func TestServedLeavesTheStoredAttribution(t *testing.T) {
	t.Parallel()
	stored := built(t)(usermessage.Report(agentmeta.ReportLoop, usermessage.Reporter{Label: "nightly"}, "n", "d", "")).Attribution().Wake
	before := *stored
	served := stored.Served()
	served.Report.Sender = "changed"
	served.Facts.Body[0].Start = 0
	if stored.Report != nil || !reflect.DeepEqual(*stored, before) {
		t.Fatalf("stored %+v", stored)
	}
	if (*agentmeta.OriginWake)(nil).Served() != nil {
		t.Fatal("served an absent attribution")
	}
	// A message read back with a served attribution shares none of it.
	message, err := usermessage.DecodeStored("n\nd", agentmeta.OriginAgentWake, nil, served)
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
	meta := agentmeta.Meta{Origin: agentmeta.OriginAgentWake, OriginWake: &agentmeta.OriginWake{Kind: string(agentmeta.ReportLoop), Label: "nightly"}, ExpandComposerCommands: true}
	projected, ok := agentmeta.ProjectMeta("mine", meta)
	if !ok || projected.OriginWake.Report == nil || projected.OriginWake.Report.Badge != "Loop · nightly" {
		t.Fatalf("served %+v", projected.OriginWake)
	}
	if _, ok := agentmeta.ProjectMeta("mine", agentmeta.Meta{Origin: store.ThreadRequestSend}); ok {
		t.Fatal("projected a row with nothing to project")
	}
}
