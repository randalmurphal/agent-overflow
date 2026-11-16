package app

import (
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// Every report kind is defined once (usermessage.ReportKinds): the row a
// client receives, the Edits selector's label and thread_show's sender are
// each read from that one definition, and say what they said when each
// reader had its own.
func TestEveryReportKindReadsTheSameEverywhere(t *testing.T) {
	t.Parallel()
	type want struct {
		descriptor usermessage.ReportDescriptor
		label      string
		transcript string
	}
	data, text := usermessage.ReportBodyData, usermessage.ReportBodyText
	cases := []struct {
		kind usermessage.ReportKind
		from usermessage.Reporter
		want want
	}{
		{usermessage.ReportLoop, usermessage.Reporter{Label: "nightly"}, want{usermessage.ReportDescriptor{
			Category: "loop", Label: "Loop", Title: "Loop wake", Sender: "nightly", Badge: "Loop · nightly", Body: data,
		}, `nightly: "data <line>"`, `loop "nightly"`}},
		{usermessage.ReportLoop, usermessage.Reporter{}, want{usermessage.ReportDescriptor{
			Category: "loop", Label: "Loop", Title: "Loop wake", Sender: "Loop wake", Badge: "Loop", Body: data,
		}, `Loop wake: "data <line>"`, `loop ""`}},
		{usermessage.ReportWatch, usermessage.Reporter{Label: "ci"}, want{usermessage.ReportDescriptor{
			Category: "watch", Label: "Watch", Title: "Watch wake", Sender: "ci", Badge: "Watch · ci", Body: data,
		}, `ci: "data <line>"`, `watch "ci"`}},
		{usermessage.ReportSend, usermessage.Reporter{}, want{usermessage.ReportDescriptor{
			Category: "script", Label: "Shell", Title: "Message from a script", Sender: "A script", Badge: "Sent by a script", Body: data,
		}, `A script: "data <line>"`, "a script (agent-overflow thread send)"}},
		{usermessage.ReportQuestion, usermessage.Reporter{Label: "Worker", ThreadID: "t-worker"}, want{usermessage.ReportDescriptor{
			Category: "worker-question", Label: "Ask", Title: "Worker's question", Sender: "Worker", Opens: "t-worker",
			Badge: "Question · Worker", Body: data,
		}, `Worker: "data <line>"`, `"Worker" (t-worker), its question`}},
		{usermessage.ReportQuestion, usermessage.Reporter{ThreadID: "t-worker"}, want{usermessage.ReportDescriptor{
			Category: "worker-question", Label: "Ask", Title: "Worker's question", Sender: "Worker's question", Opens: "t-worker",
			Badge: "Question", Body: data,
		}, `Worker's question: "data <line>"`, `"" (t-worker), its question`}},
		{usermessage.ReportApproval, usermessage.Reporter{Label: "Worker", ThreadID: "t-worker"}, want{usermessage.ReportDescriptor{
			Category: "worker-approval", Label: "Allow", Title: "Worker's approval request", Sender: "Worker", Opens: "t-worker",
			Badge: "Approval · Worker", Body: data,
		}, `Worker: "data <line>"`, `"Worker" (t-worker), its approval`}},
		{usermessage.ReportCompletion, usermessage.Reporter{Label: "make test", Computer: "desk"}, want{usermessage.ReportDescriptor{
			Category: "job", Label: "Job", Title: "Remote job finished", Sender: "make test on desk", Badge: "Remote job · make test", Body: text,
		}, `make test on desk: "data \u003cline\u003e"`, `remote job "make test" on desk`}},
		{usermessage.ReportCompletion, usermessage.Reporter{}, want{usermessage.ReportDescriptor{
			Category: "job", Label: "Job", Title: "Remote job finished", Sender: "A remote job", Badge: "Remote job", Body: text,
		}, `A remote job: "data \u003cline\u003e"`, `remote job "" on `}},
		{usermessage.ReportWorkflow, usermessage.Reporter{Label: "release"}, want{usermessage.ReportDescriptor{
			Category: "workflow", Label: "Run", Title: "Workflow wake", Sender: "release", Badge: "Workflow · release", Body: data,
		}, `release: "data <line>"`, `workflow "release"`}},
		{usermessage.ReportOpening, usermessage.Reporter{Label: "Ops"}, want{usermessage.ReportDescriptor{
			Category: "opening", Label: "Start", Title: "Assistant's opening message", Sender: "Agent Overflow", Badge: "Opening · Ops", Body: text,
		}, `Agent Overflow: "data \u003cline\u003e"`, `assistant "Ops"`}},
		{usermessage.ReportHandOff, usermessage.Reporter{Label: "Ops"}, want{usermessage.ReportDescriptor{
			Category: "handoff", Label: "Moved", Title: "New thread, same assistant", Sender: "Agent Overflow", Badge: "New thread · Ops", Body: text,
		}, `Agent Overflow: "data \u003cline\u003e"`, `handoff "Ops"`}},
	}
	covered := map[usermessage.ReportKind]bool{}
	for _, tc := range cases {
		covered[tc.kind] = true
		name := string(tc.kind) + " from " + tc.from.Label
		message := builtMessage(t)(usermessage.Report(tc.kind, tc.from, "Notice for the agent.", `"data \u003cline\u003e"`, ""))
		attribution := message.Attribution()
		stored, err := usermessage.MarshalMeta(usermessage.Meta{Origin: attribution.Origin, OriginWake: attribution.Wake, SendID: "agent-wake:x"})
		if err != nil {
			t.Fatal(err)
		}
		served, err := usermessage.FromItem(store.Item{Meta: usermessage.ProjectRowMeta(message.Text(), stored)})
		if err != nil {
			t.Fatal(err)
		}
		if got := served.OriginWake.Report; got == nil || *got != tc.want.descriptor {
			t.Errorf("%s: served %+v, want %+v", name, got, tc.want.descriptor)
		}
		decoded, err := usermessage.FromItem(store.Item{Meta: stored})
		if err != nil {
			t.Fatal(err)
		}
		if label := agentMessageLabel(message.Text(), decoded); label != tc.want.label {
			t.Errorf("%s: selector label %q, want %q", name, label, tc.want.label)
		}
		if from, ok := agentMessageSender(store.Item{Kind: "user_text", Summary: message.Text(), Meta: stored}); !ok || from != tc.want.transcript {
			t.Errorf("%s: thread_show names it %q, want %q", name, from, tc.want.transcript)
		}
	}
	for _, kind := range usermessage.ReportKinds() {
		if !covered[kind] {
			t.Errorf("report kind %q has no case", kind)
		}
	}
}
