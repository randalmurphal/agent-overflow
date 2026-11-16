package app

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
	"agent-overflow/internal/workflow/wake"
)

// factsOf is the facts a message's attribution carries, nil when it has
// none.
func factsOf(message usermessage.AgentMessage) *threadtools.MessageFacts {
	if thread := message.OriginThread(); thread != nil {
		return thread.Facts
	}
	if wake := message.OriginWake(); wake != nil {
		return wake.Facts
	}
	return nil
}

// Every message the app writes for an agent carries the facts its row is
// read by, made with its text from one value: the body the facts point at
// is the data the writer was given, the text before it is the notice the
// agent alone reads, and the headline is the one the writer chose.
func TestEveryAgentWakeWriterStoresItsFacts(t *testing.T) {
	t.Parallel()
	watchOut := newCappedText("\nbuild failed: exit 2\n", 1024)
	approval := &workerPrompt{token: "tok-1", approval: &provider.ApprovalRequest{ToolName: "Bash", Description: "Run the tests"}}
	question := &workerPrompt{token: "tok-2", question: &provider.UserInputRequest{Questions: []provider.UserInputQuestion{{Header: "Scope", Question: "Which module?"}}}}
	remote := store.RemoteWatch{ComputerID: "c-desk", RequestID: "r-1", Label: "make test", Receipt: store.RemoteJob{Workspace: "/w", State: "finished", ExitCode: 2}}
	for _, tc := range []struct {
		name     string
		message  usermessage.AgentMessage
		kind     string
		notice   string
		bodyHas  string
		headline string
	}{
		{"a loop's fire", renderAgentLoopWake(store.AgentWake{Label: "nightly", Note: "Check  the build."}, agentWakeFire{missed: 2}),
			store.AgentWakeLoop, `Loop "nightly" fired. ` + agentWakeDataNotice, `note: "Check  the build."`, "Check the build."},
		{"a watch's output", renderAgentWatchWake(store.AgentWake{Label: "ci"}, agentWakeFire{}, watchOut, cappedOutput{}),
			store.AgentWakeWatch, `Watch "ci" reported. ` + agentWakeDataNotice, `"build failed: exit 2"`, "build failed: exit 2"},
		{"an outside send", renderAgentSendWake("deploy finished\nall good"),
			usermessage.OriginWakeKindSend, agentSendWakeHeader + " " + agentWakeDataNotice, `"all good"`, "deploy finished"},
		{"a worker's approval request", workerPromptMessage("Worker", "t-worker", approval),
			usermessage.OriginWakeKindApproval, `Thread "Worker" (t-worker), which you spawned, is waiting on your answer. ` + agentWakeDataNotice,
			"token tok-1", "It asks for approval to use Bash."},
		{"a worker's question", workerPromptMessage("Worker", "t-worker", question),
			usermessage.OriginWakeKindQuestion, `Thread "Worker" (t-worker), which you spawned, is waiting on your answer. ` + agentWakeDataNotice,
			"token tok-2", "1. Scope: Which module?"},
		{"a remote job's report", remoteCompletionMessage(remote, "desk", remoteCompletionOutput{Tail: "ok"}),
			usermessage.OriginWakeKindCompletion, "Remote job finished on desk: make test", "Output (untrusted):\nok", "finished; exit code: 2"},
		{"a workflow run's wake", workflowWakeMessage("release", wake.Message{Notice: wake.DataNotice, Data: "run: \"release\""}),
			usermessage.OriginWakeKindWorkflow, wake.DataNotice, `run: "release"`, ""},
		{"an assistant's opening", assistantMessage(assistantSendID(usermessage.OriginWakeKindOpening, "a1"), "Ops", agenthome.OpeningMessage("Ops", "/config/agents/a1")),
			usermessage.OriginWakeKindOpening, "", "Ops", ""},
		{"an assistant's handoff", assistantMessage(assistantSendID(usermessage.OriginWakeKindHandOff, "a1"), "Ops", agenthome.HandOffMessage(agenthome.HandOff{Name: "Ops", PreviousThreadID: "t-old"})),
			usermessage.OriginWakeKindHandOff, "", "t-old", ""},
	} {
		facts := factsOf(tc.message)
		if tc.message.Origin() != usermessage.OriginAgentWake || tc.message.OriginWake() == nil || tc.message.OriginWake().Kind != tc.kind || facts == nil {
			t.Errorf("%s: attribution %q %+v, want an agent wake of kind %s with facts", tc.name, tc.message.Origin(), tc.message.OriginWake(), tc.kind)
			continue
		}
		text := tc.message.Text()
		body := threadtools.SpanText(text, facts.Body)
		wantBody := text
		if tc.notice != "" {
			wantBody = strings.TrimPrefix(text, tc.notice+"\n")
		}
		if body != wantBody || !strings.Contains(body, tc.bodyHas) || len(facts.Body) != 1 {
			t.Errorf("%s: body %q (spans %+v), want everything after the notice %q", tc.name, body, facts.Body, tc.notice)
		}
		if facts.Headline != tc.headline {
			t.Errorf("%s: headline %q, want %q", tc.name, facts.Headline, tc.headline)
		}
	}
}

// A headline is one line, bounded, whatever the writer passes it.
func TestAWakeHeadlineIsOneBoundedLine(t *testing.T) {
	t.Parallel()
	message := usermessage.WakeMessage(usermessage.OriginWake{Kind: store.AgentWakeLoop}, "notice", "data", "a\n  b\t"+strings.Repeat("界", usermessage.HeadlineBytes))
	headline := factsOf(message).Headline
	if strings.ContainsAny(headline, "\n\t") || !strings.HasPrefix(headline, "a b 界") || len(headline) > usermessage.HeadlineBytes {
		t.Fatalf("headline %q (%d bytes), want one line within %d bytes", headline, len(headline), usermessage.HeadlineBytes)
	}
}

// A thread's answer and the worker-stopped notice carry their facts too,
// and a digest names each thread it speaks for with the computer it is on,
// which its row opens it through.
func TestThreadWakeAndNoticeWritersStoreTheirFacts(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	rows := []store.ThreadRequest{
		{Token: "tok-1", TargetThreadID: "t-local"},
		{Token: "tok-2", TargetThreadID: "t-remote", TargetComputerID: "c-desk", TargetThreadTitle: "Remote"},
		{Token: "tok-3", TargetThreadID: "t-remote", TargetComputerID: "c-desk", TargetThreadTitle: "Remote"},
	}
	digest := app.threadWakeNote(rows[0], app.threadWakeFor(threadWakeMessage{kind: threadtools.WakeReply, text: "Same answer.", rows: rows}))
	facts := factsOf(digest)
	if digest.Origin() != usermessage.OriginAgentThread || digest.OriginThread().Kind != usermessage.OriginKindWake || facts == nil {
		t.Fatalf("digest attribution %q %+v, want an agent thread's wake with facts", digest.Origin(), digest.OriginThread())
	}
	if facts.Outcome != threadtools.WakeReply || threadtools.SpanText(digest.Text(), facts.Body) != "Same answer." ||
		!slices.Equal(facts.Tokens, []string{"tok-1", "tok-2", "tok-3"}) || len(facts.Threads) != 2 {
		t.Fatalf("digest facts = %+v", facts)
	}
	if local, remote := facts.Threads[0], facts.Threads[1]; local.ThreadID != "t-local" || local.ComputerID != "" ||
		remote.ThreadID != "t-remote" || remote.ComputerID != "c-desk" || remote.Title != "Remote" ||
		!slices.Equal(remote.Tokens, []string{"tok-2", "tok-3"}) {
		t.Fatalf("digest threads = %+v, want the remote one with its computer id", facts.Threads)
	}

	single := app.threadWakeNote(rows[1], app.threadWakeFor(threadWakeMessage{kind: threadtools.WakeErrored, text: "boom", rows: rows[1:2]}))
	if facts := factsOf(single); facts == nil || facts.Outcome != threadtools.WakeErrored || len(facts.Threads) != 0 ||
		threadtools.SpanText(single.Text(), facts.Body) != "boom" || single.OriginThread().ComputerID != "c-desk" {
		t.Fatalf("single wake %+v, facts %+v", single.OriginThread(), facts)
	}

	stopped := workerStoppedNote(usermessage.OriginThread{ThreadID: "t-worker", Title: "Worker", Kind: usermessage.OriginKindWake}, "tok-9", []string{"its watch"})
	if facts := factsOf(stopped); facts == nil || facts.Outcome != threadtools.OutcomeWorkerStopped ||
		!slices.Equal(facts.Tokens, []string{"tok-9"}) || threadtools.SpanText(stopped.Text(), facts.Body) != stopped.Text() ||
		!strings.Contains(stopped.Text(), "its watch") {
		t.Fatalf("worker-stopped notice %q, facts %+v", stopped.Text(), facts)
	}
}

// A wake the supervisor delivers is queued with its facts, so the row the
// queue writes when the agent takes it in reads them.
func TestADeliveredWakeIsQueuedWithItsFacts(t *testing.T) {
	t.Parallel()
	a, thread := heldNoticeFixture(t, "facts-queued")
	row, err := a.store.RegisterAgentWake(store.AgentWake{
		ID: "wake-board", ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "Check the board.",
		Command: "true", IntervalSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("RegisterAgentWake: %v", err)
	}
	message := renderAgentLoopWake(row, agentWakeFire{})
	if err := a.deliverAgentWake(context.Background(), row, 1, message); err != nil {
		t.Fatalf("deliverAgentWake: %v", err)
	}
	a.waitAgentNoticeWork()
	queued := durableQueueRows(t, a, thread.ID)
	if len(queued) != 1 || queued[0].Message != message.Text() {
		t.Fatalf("queue = %+v, want the wake", queued)
	}
	stored := queuedAgentMessage(queued[0])
	if stored.Origin() != usermessage.OriginAgentWake || !reflect.DeepEqual(factsOf(stored), factsOf(message)) {
		t.Fatalf("queued attribution %q %+v, want the wake's facts %+v", stored.Origin(), stored.OriginWake(), factsOf(message))
	}
}
