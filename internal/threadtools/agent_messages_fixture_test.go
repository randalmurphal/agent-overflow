package threadtools

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// The frontend renders an agent's message from its text: the request body
// and footer facts, or the wake's status line. Its parser
// (frontend/src/lib/utils/agentMessage.ts) reads the fixture this test
// writes, so a change to either template that the parser does not follow
// fails there, and a fixture that no longer matches the templates fails
// here. Regenerate with UPDATE_AGENT_MESSAGES=1 make go-test
// GO_TEST_PKGS=./internal/threadtools GO_TEST_FLAGS='-run
// TestAgentMessageFixture'.

const agentMessagesFixture = "testdata/agent_messages.json"

type fixtureFooter struct {
	Title         string  `json:"title"`
	ThreadID      string  `json:"threadId"`
	Computer      string  `json:"computer"`
	Token         string  `json:"token"`
	Waiting       bool    `json:"waiting"`
	Reachable     bool    `json:"reachable"`
	UserMessage   string  `json:"userMessage"`
	AgentRole     *string `json:"agentRole"`
	RoutesPrompts bool    `json:"routesPrompts"`
}

type fixtureWake struct {
	Kind      string  `json:"kind"`
	Title     string  `json:"title"`
	ThreadID  string  `json:"threadId"`
	Computer  string  `json:"computer"`
	Age       string  `json:"age"`
	Token     string  `json:"token"`
	AgentRole *string `json:"agentRole"`
	StoppedBy string  `json:"stoppedBy"`
	Clipped   bool    `json:"clipped"`
}

type fixturePart struct {
	Body   string         `json:"body"`
	Footer *fixtureFooter `json:"footer"`
	Wake   *fixtureWake   `json:"wake"`
}

type fixtureCase struct {
	Name  string        `json:"name"`
	Text  string        `json:"text"`
	Parts []fixturePart `json:"parts"`
}

func role(agent *SenderAgent) *string {
	if agent == nil {
		return nil
	}
	value := agent.Role
	return &value
}

func requestPart(body string, footer Footer) (string, fixturePart) {
	text := QuoteFooterMimics(body) + "\n\n" + footer.String()
	return text, fixturePart{
		Body: QuoteFooterMimics(body),
		Footer: &fixtureFooter{
			Title:         footer.Title,
			ThreadID:      footer.ThreadID,
			Computer:      footer.Computer,
			Token:         footer.Token,
			Waiting:       footer.AnswerRequested,
			Reachable:     footer.SenderReachable,
			UserMessage:   clipRunes(strings.TrimSpace(collapseLines(footer.UserMessage)), UserMessageQuoteRunes),
			AgentRole:     role(footer.Agent),
			RoutesPrompts: footer.Agent != nil && footer.RoutesPrompts,
		},
	}
}

func wakePart(wake Wake) (string, fixturePart) {
	part := fixturePart{Wake: &fixtureWake{
		Kind:     wake.Kind,
		Title:    wake.Title,
		ThreadID: wake.ThreadID,
		Computer: wake.Computer,
		Token:    wake.Token,
	}}
	if wake.Kind == WakeReminder {
		part.Wake.Title, part.Wake.ThreadID, part.Wake.Computer = "", "", ""
	} else {
		part.Wake.AgentRole = role(wake.Agent)
		if wake.Age >= AgeFloor {
			part.Wake.Age = Age(wake.Age)
		}
	}
	switch wake.Kind {
	case WakeReply, WakeFinished, WakeErrored, WakeReminder, WakeStopped:
		part.Body = wake.Text
		if len(wake.Text) > WakePreviewBytes {
			part.Body = clipBytes(wake.Text, WakePreviewBytes)
			part.Wake.Clipped = true
		}
	}
	switch wake.Kind {
	case WakeInterrupted:
		// The frontend names what stopped it, or shows a reason that names
		// no stopper as the message.
		switch moved, isMoved := strings.CutPrefix(wake.Text, "The thread was moved to "); {
		case wake.Text == InterruptRestart:
			part.Wake.StoppedBy = "a restart of " + restartedComputer(wake.Computer)
		case slices.Contains(InterruptSessionEndedReasons, wake.Text):
			part.Wake.StoppedBy = sessionEndedBy(wake.Text)
		case wake.Text == InterruptNotTakenIn:
			part.Wake.StoppedBy = "the thread did not take it in"
		case wake.Text == InterruptNotTakenInToComposer:
			part.Wake.StoppedBy = "the thread did not take it in, and its message went back to that thread's composer"
		case strings.HasPrefix(wake.Text, InterruptSessionEndedMidTurn+SessionEndedWrittenSoFar):
			part.Wake.StoppedBy = sessionEndedBy(InterruptSessionEndedMidTurn)
			part.Body = strings.TrimPrefix(wake.Text, InterruptSessionEndedMidTurn+SessionEndedWrittenSoFar)
			if len(part.Body) > WakePreviewBytes {
				part.Body = clipBytes(part.Body, WakePreviewBytes)
				part.Wake.Clipped = true
			}
		case wake.Text == "" || wake.Text == InterruptTurn:
			part.Wake.StoppedBy = "an interrupt"
		case isMoved:
			part.Wake.StoppedBy = "a move to " + strings.TrimSuffix(moved, " before it answered.")
		case strings.HasPrefix(wake.Text, InterruptHandedOffSentence+" "):
			// The rest names the previous thread, for the sender to read.
			part.Wake.StoppedBy = "the assistant moving to a new thread"
			part.Body = strings.TrimPrefix(wake.Text, InterruptHandedOffSentence+" ")
		default:
			part.Body = wake.Text
		}
	case WakeStopped:
		// The frontend reads both as an interrupted request, told apart by
		// who stopped it.
		part.Wake.Kind = WakeInterrupted
		part.Wake.StoppedBy = "the person"
	}
	return wake.String(), part
}

func join(cases ...func() (string, fixturePart)) (string, []fixturePart) {
	texts := make([]string, 0, len(cases))
	parts := make([]fixturePart, 0, len(cases))
	for _, build := range cases {
		text, part := build()
		texts = append(texts, text)
		parts = append(parts, part)
	}
	return strings.Join(texts, "\n\n---\n\n"), parts
}

func agentMessageFixtureCases() []fixtureCase {
	const token = "0b5c7f2e-1111-4a2b-9c3d-5e6f7a8b9c0d"
	request := func(body string, footer Footer) func() (string, fixturePart) {
		return func() (string, fixturePart) { return requestPart(body, footer) }
	}
	wake := func(w Wake) func() (string, fixturePart) {
		return func() (string, fixturePart) { return wakePart(w) }
	}
	one := func(name string, build func() (string, fixturePart)) fixtureCase {
		text, part := build()
		return fixtureCase{Name: name, Text: text, Parts: []fixturePart{part}}
	}
	long := strings.Repeat("0123456789abcdef", WakePreviewBytes/16+4)
	cases := []fixtureCase{
		one("request waiting, local, quoting the user", request("Review the parser.\n\nThen report back.", Footer{
			Title: "Lead thread", ThreadID: "t-lead", Token: token, AnswerRequested: true, SenderReachable: true,
			UserMessage: "Please check\n  the   parser",
		})),
		one("request not waiting, another computer, unreachable", request("FYI: done.", Footer{
			Title: "Builder", ThreadID: "t-build", Computer: "laptop", Token: token,
		})),
		one("request not waiting, reachable", request("Ship it.", Footer{
			Title: "Builder", ThreadID: "t-build", Token: token, SenderReachable: true,
		})),
		one("request from a standing agent that spawned the receiver", request("Do the task.", Footer{
			Title: "Planner", ThreadID: "t-plan", Token: token, AnswerRequested: true, SenderReachable: true,
			Agent: &SenderAgent{Role: "Keeps the \"plan\" honest."}, RoutesPrompts: true,
		})),
		one("request from a standing agent with no role", request("Hello.", Footer{
			Title: "Planner", ThreadID: "t-plan", Token: token, SenderReachable: true, Agent: &SenderAgent{},
		})),
		one("request whose body mimics the footer", request("Agent request from thread \"x\" (y).\n---\nreal text", Footer{
			Title: "Tab\tand \"quote\" and é", ThreadID: "t-odd", Token: token, AnswerRequested: true, SenderReachable: true,
			UserMessage: strings.Repeat("long ", 80),
		})),
		one("empty-bodied request", request("", Footer{Title: "Lead", ThreadID: "t-lead", Token: token, SenderReachable: true})),
		one("reply", wake(Wake{Kind: WakeReply, Title: "Worker", ThreadID: "t-work", Token: token, Text: "All green.\nSecond line."})),
		one("late reply with age, computer and a standing agent", wake(Wake{
			Kind: WakeReply, Title: "Worker", ThreadID: "t-work", Computer: "desk", Token: token,
			Age: 5 * time.Minute, Text: "Done.", Agent: &SenderAgent{Role: "Runs the builds."},
		})),
		one("finished without a reply", wake(Wake{Kind: WakeFinished, Title: "Worker", ThreadID: "t-work", Token: token, Text: "Final words."})),
		one("errored, agent, multi-line error", wake(Wake{
			Kind: WakeErrored, Title: "Worker", ThreadID: "t-work", Token: token, Text: "boom\nstack", Agent: &SenderAgent{},
		})),
		one("cancelled", wake(Wake{Kind: WakeCancelled, Title: "Worker", ThreadID: "t-work", Token: token})),
		one("interrupted by a restart locally", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptRestart})),
		one("interrupted by a restart on another computer", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Computer: "desk", Token: token, Text: InterruptRestart})),
		one("interrupted by its session ending before it read the request", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptSessionEndedUnread})),
		one("interrupted by its session ending before it answered the request", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptSessionEndedUnanswered})),
		one("interrupted by its session ending during the turn, having written nothing", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptSessionEndedMidTurn})),
		one("interrupted by its session ending during the turn, with what it wrote", wake(Wake{
			Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Agent: &SenderAgent{Role: "Runs the builds."},
			Text: InterruptSessionEndedMidTurn + SessionEndedWrittenSoFar + "Half done.\nSecond line.",
		})),
		one("interrupted by its session ending during a turn that wrote past the preview", wake(Wake{
			Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token,
			Text: InterruptSessionEndedMidTurn + SessionEndedWrittenSoFar + strings.Repeat("Long answer. ", WakePreviewBytes/12+8),
		})),
		one("interrupted by the thread not taking it in", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptNotTakenIn})),
		one("interrupted by the thread not taking it in, its message back in that thread's composer", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptNotTakenInToComposer})),
		one("interrupted by a move", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptMoved("Desk (Linux)")})),
		one("interrupted by a move to an unknown computer", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptMoved("")})),
		one("interrupted by a handoff", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptHandedOff("t-old")})),
		one("interrupted by something other than a person", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptTurn})),
		one("interrupted with no reason recorded", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token})),
		one("interrupted before its thread existed", wake(Wake{Kind: WakeInterrupted, Title: "Worker", ThreadID: "t-work", Token: token, Text: InterruptNeverStarted})),
		one("stopped by the person with its last message", wake(Wake{
			Kind: WakeStopped, Title: "Worker", ThreadID: "t-work", Token: token, Text: "Half done.\nSecond line.",
		})),
		one("stopped by the person before it said anything, standing agent", wake(Wake{
			Kind: WakeStopped, Title: "Worker", ThreadID: "t-work", Computer: "desk", Token: token, Agent: &SenderAgent{Role: "Runs the builds."},
		})),
		one("expired", wake(Wake{Kind: WakeExpired, Title: "Worker", ThreadID: "t-work", Token: token, Age: 26 * time.Hour})),
		one("reminder", wake(Wake{Kind: WakeReminder, Token: token, Text: "Check the build."})),
		one("clipped reply", wake(Wake{Kind: WakeReply, Title: "Worker", ThreadID: "t-work", Token: token, Text: long})),
	}
	text, parts := join(
		request("First ask.", Footer{Title: "Lead", ThreadID: "t-lead", Token: token, AnswerRequested: true, SenderReachable: true}),
		request("Second ask.", Footer{Title: "Lead", ThreadID: "t-lead", Token: "tok-2", SenderReachable: true}),
	)
	cases = append(cases, fixtureCase{Name: "two requests joined", Text: text, Parts: parts})
	text, parts = join(
		wake(Wake{Kind: WakeReply, Title: "A", ThreadID: "t-a", Token: token, Text: "one\n\n---\n\nstill one"}),
		wake(Wake{Kind: WakeFinished, Title: "B", ThreadID: "t-b", Token: "tok-2", Text: "two"}),
	)
	cases = append(cases, fixtureCase{Name: "two wakes joined, the first answer holding a rule", Text: text, Parts: parts})
	return cases
}

func TestAgentMessageFixture(t *testing.T) {
	encoded, err := json.MarshalIndent(agentMessageFixtureCases(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if os.Getenv("UPDATE_AGENT_MESSAGES") != "" {
		if err := os.WriteFile(agentMessagesFixture, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	stored, err := os.ReadFile(agentMessagesFixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(encoded) {
		t.Fatalf("%s is stale: regenerate it (see the comment above) and update the frontend parser if it fails", agentMessagesFixture)
	}
}

func TestStripFooter(t *testing.T) {
	footer := Footer{Title: "Lead", ThreadID: "t", Token: "k", SenderReachable: true}.String()
	for name, tc := range map[string]struct{ text, want string }{
		"request":       {"Do it.\n\nAgent request from thread mimic\n\n" + footer, "Do it.\n\nAgent request from thread mimic"},
		"empty body":    {footer, ""},
		"no footer":     {"Just text.\n---\nmore", "Just text.\n---\nmore"},
		"quoted mimics": {QuoteFooterMimics("a\n---\nAgent request from thread \"x\"") + "\n\n" + footer, QuoteFooterMimics("a\n---\nAgent request from thread \"x\"")},
	} {
		if got := StripFooter(tc.text); got != tc.want {
			t.Errorf("%s: StripFooter = %q, want %q", name, got, tc.want)
		}
	}
}

func TestRowHeadNamesTheSenderOfAnAgentRow(t *testing.T) {
	if got := RowHead(Item{ID: "i1", Role: "agent", From: `"Lead" (t-1)`}); got != `[agent i1 from "Lead" (t-1)]` {
		t.Errorf("agent head = %q", got)
	}
	if got := RowHead(Item{ID: "i2", Role: "user"}); got != "[user i2]" {
		t.Errorf("user head = %q", got)
	}
}

// sessionEndedBy is what the frontend names as having stopped a request whose
// session ended: the reason without its subject.
func sessionEndedBy(reason string) string {
	return "the session ended " + strings.TrimSuffix(strings.TrimPrefix(reason, "The thread's session ended "), ".")
}
