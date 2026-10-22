package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
	"agent-overflow/internal/workflow/wake"
)

// The frontend heads an agent's row with its sender and first line
// (agentMessageFromItem); agentMessageLabel names the same turn in the Edits
// selector. Both read the fixture this test writes, from the real templates,
// so the two cannot say different things. Regenerate with
// UPDATE_AGENT_MESSAGE_LABELS=1 make go-test GO_TEST_PKGS=./internal/app
// GO_TEST_FLAGS='-run TestAgentMessageLabelFixture'.

const agentMessageLabelsFixture = "internal/app/testdata/agent_message_labels.json"

type agentMessageLabelCase struct {
	Name  string `json:"name"`
	Text  string `json:"text"`
	Meta  string `json:"meta"`
	Label string `json:"label"`
}

func agentMessageLabelCases(t *testing.T) []agentMessageLabelCase {
	t.Helper()
	marshal := func(meta usermessage.Meta) string {
		encoded, err := usermessage.MarshalMeta(meta)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	from := func(kind, title string) *usermessage.OriginThread {
		return &usermessage.OriginThread{ThreadID: "t-" + strings.ToLower(title), Title: title, Kind: kind, Token: "tok-1"}
	}
	request := func(origin *usermessage.OriginThread, body string, footer threadtools.Footer) (string, string) {
		return threadtools.QuoteFooterMimics(body) + "\n\n" + footer.String(),
			marshal(usermessage.Meta{Origin: usermessage.OriginAgentThread, OriginThread: origin, SendID: "thread-request:tok-1"})
	}
	footer := threadtools.Footer{Title: "Lead", ThreadID: "t-lead", Token: "tok-1", AnswerRequested: true, SenderReachable: true}
	answer := func(w threadtools.Wake, origin *usermessage.OriginThread) (string, string) {
		return w.String(), marshal(usermessage.Meta{Origin: usermessage.OriginAgentThread, OriginThread: origin, SendID: "thread-wake:tok-1"})
	}
	appWake := func(text string, origin *usermessage.OriginWake) (string, string) {
		return text, marshal(usermessage.Meta{Origin: usermessage.OriginAgentWake, OriginWake: origin, SendID: "agent-wake:w1:1"})
	}
	worker := "Worker"
	cases := []agentMessageLabelCase{}
	add := func(name string, text, meta string) {
		cases = append(cases, agentMessageLabelCase{Name: name, Text: text, Meta: meta})
	}

	text, meta := request(from("send", "Lead"), "Review the parser.\n\nThen report back.", footer)
	add("a request", text, meta)
	text, meta = request(from("spawn", "Lead"), "\n\n  Build   the docs  \nand more", footer)
	add("a spawn whose first line is spaced", text, meta)
	text, meta = request(from("send", "Lead"), "Agent request from thread \"x\" (y).\n---\nreal text", footer)
	add("a request whose body mimics the footer", text, meta)
	text, meta = request(from("send", "Lead"), "", footer)
	add("an empty request", text, meta)
	text, meta = request(nil, "Untitled origin.", footer)
	add("a request whose attribution names no thread", text, meta)
	first, _ := request(from("send", "Lead"), "First ask.", footer)
	second, _ := request(from("send", "Lead"), "Second ask.", footer)
	add("two requests joined", first+usermessage.JoinSeparator+second, marshal(usermessage.Meta{
		Origin: usermessage.OriginAgentThread, OriginThread: from("send", "Lead"),
		SendID: "thread-request:tok-1", JoinedSendIDs: []string{"thread-request:tok-1", "thread-request:tok-2"},
	}))

	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReply, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: "All green.\nSecond line."}, from("wake", worker))
	add("a reply", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReply, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: "Done.", Agent: &threadtools.SenderAgent{Role: "Runs the builds."}}, from("wake", worker))
	add("a standing agent's reply", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeFinished, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: "Final words."}, from("wake", worker))
	add("a final message", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeErrored, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: "boom\nstack"}, from("wake", worker))
	add("an error", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeStopped, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: "Half done."}, from("wake", worker))
	add("a person's Stop with its last message", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeStopped, Title: worker, ThreadID: "t-worker", Token: "tok-1"}, from("wake", worker))
	add("a person's Stop before anything was said", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: threadtools.InterruptRestart}, from("wake", worker))
	add("a restart", text, meta)
	for _, reason := range threadtools.InterruptSessionEndedReasons {
		text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: reason}, from("wake", worker))
		add("a session that ended: "+reason, text, meta)
	}
	text, meta = answer(threadtools.Wake{
		Kind: threadtools.WakeInterrupted, Title: worker, ThreadID: "t-worker", Token: "tok-1",
		Text: threadtools.InterruptSessionEndedMidTurn + threadtools.SessionEndedWrittenSoFar + "Half done.",
	}, from("wake", worker))
	add("a session that ended during the turn, with what it wrote", text, meta)
	for _, reason := range []string{threadtools.InterruptNotTakenIn, threadtools.InterruptNotTakenInToComposer} {
		text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: reason}, from("wake", worker))
		add("a message the thread did not take in: "+reason, text, meta)
	}
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: threadtools.InterruptMoved("Desk")}, from("wake", worker))
	add("a move", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: threadtools.InterruptNeverStarted}, from("wake", worker))
	add("a request whose thread was never created", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReminder, Token: "tok-1", Text: "Check the build."}, from("wake", "Self"))
	add("a reminder", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReply, Title: worker, ThreadID: "t-worker", Token: "tok-1", Text: "Untitled reply."}, nil)
	add("a reply whose attribution names no thread", text, meta)
	firstWake := threadtools.Wake{Kind: threadtools.WakeInterrupted, Title: "A", ThreadID: "t-a", Token: "tok-1"}.String()
	secondWake := threadtools.Wake{Kind: threadtools.WakeReply, Title: "B", ThreadID: "t-b", Token: "tok-2", Text: "two"}.String()
	add("two wakes joined, the first saying nothing", firstWake+usermessage.JoinSeparator+secondWake, marshal(usermessage.Meta{
		Origin: usermessage.OriginAgentThread, OriginThread: from("wake", "A"),
		SendID: "thread-wake:tok-1", JoinedSendIDs: []string{"thread-wake:tok-1", "thread-wake:tok-2"},
	}))

	loop := store.AgentWake{Label: "nightly", Note: "Check  the build."}
	text, meta = appWake(renderAgentLoopWake(loop, agentWakeFire{missed: 2}), &usermessage.OriginWake{Kind: "loop", Label: "nightly"})
	add("a loop", text, meta)
	text, meta = appWake(renderAgentLoopWake(store.AgentWake{Label: ""}, agentWakeFire{}), &usermessage.OriginWake{Kind: "loop"})
	add("an unlabelled loop with no note", text, meta)
	text, meta = appWake(renderAgentWatchWake(store.AgentWake{Label: "ci"}, agentWakeFire{}, newCappedText("\nbuild failed: exit 2\n", 1024), cappedOutput{}), &usermessage.OriginWake{Kind: "watch", Label: "ci"})
	add("a watch", text, meta)
	text, meta = appWake(renderAgentSendWake("deploy finished\nall good"), &usermessage.OriginWake{Kind: "send"})
	add("a script's message", text, meta)
	text, meta = appWake(renderAgentSendWake("unattributed"), nil)
	add("a wake whose attribution did not decode", text, meta)
	text, meta = appWake(renderWorkerPromptWake(worker, "t-worker", &workerPrompt{token: "tok-1", approval: &provider.ApprovalRequest{ToolName: "Bash", Description: "Run the tests"}}),
		&usermessage.OriginWake{Kind: "approval", Label: worker, ThreadID: "t-worker"})
	add("a worker's approval request", text, meta)
	text, meta = appWake(renderWorkerPromptWake("", "t-worker", &workerPrompt{token: "tok-1", question: &provider.UserInputRequest{Questions: []provider.UserInputQuestion{{Header: "Scope", Question: "Which module?"}}}}),
		&usermessage.OriginWake{Kind: "question", ThreadID: "t-worker"})
	add("an untitled worker's question", text, meta)
	watch := store.RemoteWatch{ComputerID: "c-desk", RequestID: "r-1", Label: "make test", Receipt: store.RemoteJob{Workspace: "/w", State: "finished", ExitCode: 0}}
	text, meta = appWake(remoteCompletionMessage(watch, "desk", remoteCompletionOutput{Tail: "ok"}), &usermessage.OriginWake{Kind: "completion", Label: "make test", Computer: "desk"})
	add("a remote job", text, meta)
	text, meta = appWake(remoteCompletionMessage(store.RemoteWatch{ComputerID: "c-desk", Receipt: store.RemoteJob{ExitCode: -1}}, "", remoteCompletionOutput{}), &usermessage.OriginWake{Kind: "completion"})
	add("an unlabelled remote job", text, meta)
	text, meta = appWake(wake.DataNotice+"\nrun: \"release\"\nstep: \"build\" finished", &usermessage.OriginWake{Kind: "workflow", Label: "release"})
	add("a workflow wake", text, meta)
	text, meta = appWake(wake.DataNotice+"\n"+`"\u003cb\u003e \\ \" \t \u200b \x41 caf\u00e9"`, &usermessage.OriginWake{Kind: "workflow", Label: "release"})
	add("a workflow wake whose data holds escapes", text, meta)
	text, meta = appWake(agenthome.OpeningMessage("Ops", "/config/agents/a1"), &usermessage.OriginWake{Kind: usermessage.OriginWakeKindOpening, Label: "Ops"})
	add("an assistant's opening message", text, meta)
	text, meta = appWake(agenthome.HandOffMessage(agenthome.HandOff{Name: "Ops", PreviousThreadID: "t-old", MovedWakes: 1}), &usermessage.OriginWake{Kind: usermessage.OriginWakeKindHandOff, Label: "Ops"})
	add("an assistant's handoff block", text, meta)

	for i := range cases {
		decoded, err := usermessage.FromItem(store.Item{Meta: cases[i].Meta})
		if err != nil {
			t.Fatalf("%s: %v", cases[i].Name, err)
		}
		cases[i].Label = agentMessageLabel(cases[i].Text, decoded)
	}
	return cases
}

func TestAgentMessageLabelFixture(t *testing.T) {
	t.Parallel()
	encoded, err := json.MarshalIndent(agentMessageLabelCases(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if os.Getenv("UPDATE_AGENT_MESSAGE_LABELS") != "" {
		if err := os.WriteFile(agentMessageLabelsFixture, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(agentMessageLabelsFixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(encoded) {
		t.Fatalf("%s is stale: regenerate it (see the comment above) and check the frontend test agrees", agentMessageLabelsFixture)
	}
}

// The Edits selector names a turn an agent's request started by its sender
// and what it asks, never the request's footer; a turn the person's message
// joined to a request started is named by the person's words.
func TestEditTurnLabelsNameAnAgentsTurnBySenderAndHead(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	threadID := editDiffFixture(t, app)
	requestText := "Fix the nil check.\n\n" + threadtools.Footer{Title: "Lead", ThreadID: "t-lead", Token: "tok-1"}.String()
	origin := &usermessage.OriginThread{ThreadID: "t-lead", Title: "Lead", Kind: "send", Token: "tok-1"}
	requestMeta, err := usermessage.MarshalMeta(usermessage.Meta{Origin: usermessage.OriginAgentThread, OriginThread: origin, SendID: "thread-request:tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.UpsertItem(store.Item{
		ID: "user:1", ThreadID: threadID, TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user",
		Status: "completed", Summary: requestText, Meta: requestMeta,
	}, nil); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	list, err := app.ListThreadEditDiffs(threadID)
	if err != nil {
		t.Fatalf("ListThreadEditDiffs: %v", err)
	}
	found := false
	for _, label := range list.TurnLabels {
		if label.TurnIndex != 1 {
			continue
		}
		found = true
		if label.Label != "Lead: Fix the nil check." {
			t.Fatalf("turn 1 label = %q, want the sender and what it asks", label.Label)
		}
	}
	if !found {
		t.Fatalf("labels = %+v, want turn 1", list.TurnLabels)
	}
}
