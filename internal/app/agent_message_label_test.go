package app

import (
	"encoding/json"
	"os"
	"strconv"
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
	// joined is the row a join makes of its members (usermessage.Join).
	joined := func(texts []string, metas []string) (string, string) {
		members := make([]usermessage.JoinMember, len(texts))
		for i := range texts {
			decoded, err := usermessage.FromItem(store.Item{Meta: metas[i]})
			if err != nil {
				t.Fatal(err)
			}
			members[i] = usermessage.JoinMember{Meta: decoded, Text: texts[i]}
		}
		text, meta := usermessage.Join(members)
		return text, marshal(meta)
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
	first, firstMeta := request(from("send", "Lead"), "First ask.", footer)
	second, secondMeta := request(from("send", "Lead"), "Second ask.", footer)
	text, meta = joined([]string{first, second}, []string{firstMeta, secondMeta})
	add("two requests joined", text, meta)

	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReply, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: "All green.\nSecond line."}, from("wake", worker))
	add("a reply", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReply, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}, Agent: &threadtools.SenderAgent{Role: "Runs the builds."}}}, Text: "Done."}, from("wake", worker))
	add("a standing agent's reply", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeFinished, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: "Final words."}, from("wake", worker))
	add("a final message", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeErrored, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: "boom\nstack"}, from("wake", worker))
	add("an error", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeStopped, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: "Half done."}, from("wake", worker))
	add("a person's Stop with its last message", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeStopped, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}}, from("wake", worker))
	add("a person's Stop before anything was said", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: threadtools.InterruptRestart}, from("wake", worker))
	add("a restart", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: stopByWorkflow().cause}, from("wake", worker))
	add("a stop with its recorded cause", text, meta)
	for _, reason := range threadtools.InterruptSessionEndedReasons {
		text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: reason}, from("wake", worker))
		add("a session that ended: "+reason, text, meta)
	}
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: threadtools.InterruptSessionEndedMidTurn + threadtools.SessionEndedWrittenSoFar + "Half done."}, from("wake", worker))
	add("a session that ended during the turn, with what it wrote", text, meta)
	for _, reason := range []string{threadtools.InterruptNotTakenIn, threadtools.InterruptNotTakenInToComposer} {
		text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: reason}, from("wake", worker))
		add("a message the thread did not take in: "+reason, text, meta)
	}
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: threadtools.InterruptMoved("Desk")}, from("wake", worker))
	add("a move", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: threadtools.InterruptNeverStarted}, from("wake", worker))
	add("a request whose thread was never created", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReminder, Sources: []threadtools.WakeSource{{Tokens: []string{"tok-1"}}}, Text: "Check the build."}, from("wake", "Self"))
	add("a reminder", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeErrored, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}}, from("wake", worker))
	add("an error with no text", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeFinished, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1", "tok-2"}}}, Text: "Did both."}, from("wake", worker))
	add("one turn answering two requests", text, meta)
	digest := []threadtools.WakeSource{
		{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}, Agent: &threadtools.SenderAgent{Role: "Runs the builds."}},
		{Title: "Lexer", ThreadID: "t-lexer", Computer: "desk", Tokens: []string{"tok-2", "tok-3"}},
	}
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReply, Sources: digest, Text: "Same answer."}, from("wake", worker))
	add("a digest of the same reply", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeExpired, Sources: digest}, from("wake", worker))
	add("a digest with no text", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReply, Sources: digest, Text: "- No findings.\n- Nothing else."}, from("wake", worker))
	add("a digest whose shared reply is a Markdown list", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: digest, Text: threadtools.InterruptSessionEndedMidTurn + threadtools.SessionEndedWrittenSoFar + "Half done."}, from("wake", worker))
	add("a digest of sessions that ended during the turn", text, meta)
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: digest, Text: stopByWorkflow().cause}, from("wake", worker))
	add("a digest of stops with their recorded cause", text, meta)
	// A request made in an assistant's past thread names it after the status
	// line, or after a digest's list; the label reads past it.
	for _, w := range []threadtools.Wake{
		{Kind: threadtools.WakeReply, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: "All green."},
		{Kind: threadtools.WakeReminder, Sources: []threadtools.WakeSource{{Tokens: []string{"tok-1"}}}, Text: "Check the build."},
		{Kind: threadtools.WakeStopped, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}},
		{Kind: threadtools.WakeStopped, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: "Half done."},
		{Kind: threadtools.WakeInterrupted, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: threadtools.InterruptSessionEndedUnanswered},
		{Kind: threadtools.WakeReply, Sources: digest, Text: "Same answer."},
		{Kind: threadtools.WakeInterrupted, Sources: digest, Text: threadtools.InterruptSessionEndedUnread},
	} {
		w.MadeIn, w.Now = "t-old", "t-new"
		text, meta = answer(w, from("wake", worker))
		add("made in a past thread: "+w.Kind+" from "+strconv.Itoa(len(w.Sources))+" sources, "+firstNonEmpty(w.Text, "no text"), text, meta)
	}
	text, meta = answer(threadtools.Wake{Kind: threadtools.WakeReply, Sources: []threadtools.WakeSource{{Title: worker, ThreadID: "t-worker", Tokens: []string{"tok-1"}}}, Text: "Untitled reply."}, nil)
	add("a reply whose attribution names no thread", text, meta)
	firstWake, firstWakeMeta := answer(threadtools.Wake{Kind: threadtools.WakeInterrupted, Sources: []threadtools.WakeSource{{Title: "A", ThreadID: "t-a", Tokens: []string{"tok-1"}}}}, from("wake", "A"))
	secondWake, secondWakeMeta := answer(threadtools.Wake{Kind: threadtools.WakeReply, Sources: []threadtools.WakeSource{{Title: "B", ThreadID: "t-b", Tokens: []string{"tok-2"}}}, Text: "two"}, from("wake", "B"))
	text, meta = joined([]string{firstWake, secondWake}, []string{firstWakeMeta, secondWakeMeta})
	add("two wakes joined, the first saying nothing", text, meta)
	// The first answer quotes a whole wake after a rule; only the recorded
	// lengths tell it from the next member.
	quoting, quotingMeta := answer(threadtools.Wake{Kind: threadtools.WakeReply, Sources: []threadtools.WakeSource{{Title: "A", ThreadID: "t-a", Tokens: []string{"tok-1"}}}, Text: "Example:" + usermessage.JoinSeparator + "Reply from \"B\" (t-b) [token tok-2]:\nnot B's answer"}, from("wake", "A"))
	text, meta = joined([]string{quoting, secondWake}, []string{quotingMeta, secondWakeMeta})
	add("two wakes joined, the first answer quoting a wake", text, meta)
	// A loop's wake joined ahead of an answer heads the row as a loop.
	loopFirst, loopFirstMeta := appWake(renderAgentLoopWake(store.AgentWake{Label: "nightly", Note: "Go."}, agentWakeFire{}), &usermessage.OriginWake{Kind: "loop", Label: "nightly"})
	text, meta = joined([]string{loopFirst, secondWake}, []string{loopFirstMeta, secondWakeMeta})
	add("a loop's wake joined with an answer", text, meta)

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

// TestAgentMessageLabelQuotesTheAnswerNotTheRouting: the line a wake carries
// for a request made in an assistant's past thread is routing, never the
// answer the label quotes, and a digest lists only as many threads as it
// counts, so a shared answer that is a Markdown list is still the answer.
func TestAgentMessageLabelQuotesTheAnswerNotTheRouting(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"made in a past thread: reply from 1 sources, All green.":                                           "Worker: All green.",
		"made in a past thread: reminder from 1 sources, Check the build.":                                  "This thread: Check the build.",
		"made in a past thread: stopped from 1 sources, no text":                                            "Worker",
		"made in a past thread: stopped from 1 sources, Half done.":                                         "Worker: Half done.",
		"made in a past thread: interrupted from 1 sources, " + threadtools.InterruptSessionEndedUnanswered: "Worker",
		"made in a past thread: reply from 2 sources, Same answer.":                                         "2 threads: Same answer.",
		"made in a past thread: interrupted from 2 sources, " + threadtools.InterruptSessionEndedUnread:     "2 threads: Worker, Lexer",
		"a digest whose shared reply is a Markdown list":                                                    "2 threads: - No findings.",
	}
	seen := 0
	for _, tc := range agentMessageLabelCases(t) {
		expected, ok := want[tc.Name]
		if !ok {
			continue
		}
		seen++
		decoded, err := usermessage.FromItem(store.Item{Meta: tc.Meta})
		if err != nil {
			t.Fatal(err)
		}
		if got := agentMessageLabel(tc.Text, decoded); got != expected {
			t.Errorf("%s: label = %q, want %q", tc.Name, got, expected)
		}
	}
	if seen != len(want) {
		t.Fatalf("found %d of the %d cases", seen, len(want))
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
