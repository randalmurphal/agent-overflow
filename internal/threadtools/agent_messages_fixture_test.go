package threadtools

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The frontend renders an agent's message from the facts its writer stored
// with it (MessageFacts): each case is a message's text, the attribution
// and facts Footer.Message, Wake.Message or PlainMessage wrote with it, and
// what the row shows, which this test derives from the template's inputs on
// its own and holds the facts to. frontend/src/lib/utils/agentMessage.test.ts
// renders every case as a client receives it (internal/usermessage's
// TestServedRowsFixture), and internal/usermessage's
// TestDescribeReadsMainsTextAsItsFactsSay reads the cases marked legacy,
// whose text main's templates also wrote, from their text alone, as a row
// written before the facts. Regenerate with UPDATE_AGENT_MESSAGES=1 make
// go-test GO_TEST_PKGS=./internal/threadtools GO_TEST_FLAGS='-run
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
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	ThreadID string `json:"threadId"`
	Computer string `json:"computer"`
	Age      string `json:"age"`
	// Token is the first of Tokens, the one a clipped body points at.
	Token     string   `json:"token"`
	Tokens    []string `json:"tokens"`
	AgentRole *string  `json:"agentRole"`
	StoppedBy string   `json:"stoppedBy"`
	Clipped   bool     `json:"clipped"`
	// MadeIn and Now name the assistant's past thread the request was made
	// in and its current thread, after New Thread, Same Assistant.
	MadeIn string `json:"madeIn"`
	Now    string `json:"now"`
	// Threads lists a digest's threads; a wake for one thread names it in
	// the fields above and lists none.
	Threads []fixtureWakeThread `json:"threads"`
}

type fixtureWakeThread struct {
	Title      string   `json:"title"`
	ThreadID   string   `json:"threadId"`
	Computer   string   `json:"computer"`
	ComputerID string   `json:"computerId"`
	Age        string   `json:"age"`
	Tokens     []string `json:"tokens"`
	AgentRole  *string  `json:"agentRole"`
}

type fixturePart struct {
	Body   string         `json:"body"`
	Footer *fixtureFooter `json:"footer"`
	Wake   *fixtureWake   `json:"wake"`
}

// fixtureOrigin is the attribution a row of the message carries, in
// agentmeta.OriginThread's JSON shape, facts included.
type fixtureOrigin struct {
	ComputerID   string       `json:"computerId,omitempty"`
	ComputerName string       `json:"computerName,omitempty"`
	ThreadID     string       `json:"threadId"`
	Title        string       `json:"title,omitempty"`
	Token        string       `json:"token,omitempty"`
	Kind         string       `json:"kind"`
	Facts        MessageFacts `json:"facts"`
}

// fixtureCase is a row's text, each of its messages' attribution as the
// writer stored it, and what the row shows of each. Members is each joined
// member's length in bytes, as agentmeta.JoinedPart records it, so the
// frontend splits the row the way a stored row's meta tells it to. Legacy
// marks a text main's templates also wrote, which a row written before the
// facts holds.
type fixtureCase struct {
	Name    string          `json:"name"`
	Text    string          `json:"text"`
	Members []int           `json:"members"`
	Legacy  bool            `json:"legacy"`
	Origins []fixtureOrigin `json:"origins"`
	Parts   []fixturePart   `json:"parts"`
}

// fixtureMessage is one message of a case: its text, its attribution, what
// its row shows, and whether main's templates wrote the same text.
type fixtureMessage struct {
	text   string
	origin fixtureOrigin
	part   fixturePart
	legacy bool
}

func role(agent *SenderAgent) *string {
	if agent == nil {
		return nil
	}
	value := agent.Role
	return &value
}

// computerID stands for the id of the computer a fixture names, which the
// text never states.
func computerID(name string) string {
	if name == "" {
		return ""
	}
	return "computer-" + name
}

func requestPart(body string, footer Footer) fixtureMessage {
	text, facts := footer.Message(body)
	return fixtureMessage{
		text: text,
		origin: fixtureOrigin{
			ComputerID: computerID(footer.Computer), ComputerName: footer.Computer, ThreadID: footer.ThreadID,
			Title: footer.Title, Token: footer.Token, Kind: "send", Facts: facts,
		},
		// main's footer had no standing-agent or routing line: its
		// Footer.String wrote the marker line, the answer line and the
		// quoted user message, nothing else (git show
		// origin/main:internal/threadtools/footer.go, Footer.String).
		legacy: footer.Agent == nil && !footer.RoutesPrompts,
		part: fixturePart{
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
				RoutesPrompts: footer.RoutesPrompts,
			},
		},
	}
}

func fixtureAge(age time.Duration) string {
	if age < AgeFloor {
		return ""
	}
	return Age(age)
}

// mainWakeKinds are the wakes whose text main's templates wrote as these
// do, for a thread that is no standing agent and a request made where it
// is answered. main had no WakeStopped, and its interrupted and cancelled
// status lines differ from these ("was interrupted: ", "was cancelled."
// with no text): usermessage's legacy reader test reads those in main's
// words.
var mainWakeKinds = []string{WakeReply, WakeFinished, WakeErrored, WakeExpired, WakeReminder}

func wakePart(wake Wake) fixtureMessage {
	text, facts := wake.Message()
	first := wake.source()
	if len(wake.Sources) > 1 {
		first = wake.Sources[0]
	}
	message := fixtureMessage{
		text: text,
		origin: fixtureOrigin{
			ComputerID: first.ComputerID, ComputerName: first.Computer, ThreadID: first.ThreadID,
			Title: first.Title, Token: wake.firstToken(), Kind: "wake", Facts: facts,
		},
		// main attributed a wake for several threads to the app
		// (threadWakeAttribution in git show
		// origin/main:internal/app/app_thread_tools_wake_message.go), so no
		// agent row holds main's digest; its Wake had no MadeIn and its
		// WakeSource no Agent (origin/main:internal/threadtools/footer.go).
		legacy: len(wake.Sources) <= 1 && wake.MadeIn == "" && !slices.ContainsFunc(wake.Sources, func(source WakeSource) bool { return source.Agent != nil }) &&
			(slices.Contains(mainWakeKinds, wake.Kind) || wake.Kind == WakeCancelled && wake.Text == ""),
	}
	part := fixturePart{Wake: &fixtureWake{Kind: wake.Kind, Token: wake.firstToken(), Tokens: []string{}, Threads: []fixtureWakeThread{}, MadeIn: wake.MadeIn, Now: wake.Now}}
	for _, source := range wake.Sources {
		part.Wake.Tokens = append(part.Wake.Tokens, source.Tokens...)
	}
	restarted := "this computer"
	switch {
	case len(wake.Sources) > 1:
		for _, source := range wake.Sources {
			part.Wake.Threads = append(part.Wake.Threads, fixtureWakeThread{
				Title: source.Title, ThreadID: source.ThreadID, Computer: source.Computer, ComputerID: source.ComputerID,
				Age: fixtureAge(source.Age), Tokens: source.Tokens, AgentRole: role(source.Agent),
			})
		}
		restarted = wake.restartedComputers()
	case len(wake.Sources) == 1 && wake.Kind != WakeReminder:
		source := wake.Sources[0]
		part.Wake.Title, part.Wake.ThreadID, part.Wake.Computer = source.Title, source.ThreadID, source.Computer
		part.Wake.AgentRole = role(source.Agent)
		part.Wake.Age = fixtureAge(source.Age)
		restarted = restartedComputer(source.Computer)
	}
	switch wake.Kind {
	case WakeReply, WakeFinished, WakeErrored, WakeReminder, WakeStopped:
		part.Body = wake.Text
		if len(wake.Text) > WakePreviewBytes {
			part.Body = clipBytes(wake.Text, WakePreviewBytes)
			part.Wake.Clipped = true
		}
	case WakeCancelled:
		// Who cancelled it, when the cancel was made for the caller.
		part.Body = wake.Text
	}
	switch wake.Kind {
	case WakeInterrupted:
		// The frontend names what stopped it, or shows a reason that names
		// no stopper as the message.
		switch moved, isMoved := strings.CutPrefix(wake.Text, "The thread was moved to "); {
		case wake.Text == InterruptRestart:
			part.Wake.StoppedBy = "a restart of " + restarted
		case slices.Contains(InterruptSessionEndedReasons, wake.Text):
			part.Wake.StoppedBy = wantSessionEndedBy(wake.Text)
		case wake.Text == InterruptNotTakenIn:
			part.Wake.StoppedBy = "the thread did not take it in"
		case wake.Text == InterruptNotTakenInToComposer:
			part.Wake.StoppedBy = "the thread did not take it in, and its message went back to that thread's composer"
		case strings.HasPrefix(wake.Text, InterruptSessionEndedMidTurn+SessionEndedWrittenSoFar):
			part.Wake.StoppedBy = wantSessionEndedBy(InterruptSessionEndedMidTurn)
			part.Body = strings.TrimPrefix(wake.Text, InterruptSessionEndedMidTurn+SessionEndedWrittenSoFar)
			if len(part.Body) > WakePreviewBytes {
				part.Body = clipBytes(part.Body, WakePreviewBytes)
				part.Wake.Clipped = true
			}
		case wake.Text == "" || wake.Text == InterruptTurn:
			part.Wake.StoppedBy = "an interrupt"
		case wake.Text == WakePersonStopped:
			part.Wake.StoppedBy = "the user"
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
		part.Wake.StoppedBy = "the user"
	}
	message.part = part
	return message
}

// join is the row a provider joins messages into, as usermessage.Join
// records it.
func join(name string, messages ...fixtureMessage) fixtureCase {
	joined := fixtureCase{Name: name, Legacy: true}
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, message.text)
		joined.Members = append(joined.Members, len(message.text))
		joined.Origins = append(joined.Origins, message.origin)
		joined.Parts = append(joined.Parts, message.part)
		// main recorded no joined members: a joined row of its is read
		// whole, which tells requests apart by their footers and nothing
		// else apart.
		joined.Legacy = joined.Legacy && message.legacy && (len(messages) == 1 || message.part.Footer != nil)
	}
	joined.Text = strings.Join(texts, "\n\n---\n\n")
	return joined
}

// plainPart is a thread's message with no footer (PlainMessage): a
// spawner's answer to its worker's async questions, which answers none of
// the receiver's requests. main wrote none.
func plainPart(body string, from fixtureOrigin) fixtureMessage {
	text, facts := PlainMessage(body)
	from.Facts = facts
	return fixtureMessage{text: text, origin: from, part: fixturePart{Body: strings.TrimSpace(body)}}
}

func agentMessageFixtureCases() []fixtureCase {
	const token = "0b5c7f2e-1111-4a2b-9c3d-5e6f7a8b9c0d"
	request, wake, one := requestPart, wakePart, join
	long := strings.Repeat("0123456789abcdef", WakePreviewBytes/16+4)
	digestSources := []WakeSource{
		{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}},
		{Title: "Lexer", ThreadID: "t-lex", Computer: "desk", ComputerID: "computer-desk", Tokens: []string{"tok-2", "tok-3"}, Age: 3 * time.Hour},
	}
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
		one("request from an ordinary thread that spawned the receiver", request("Do the task.", Footer{
			Title: "Planner", ThreadID: "t-plan", Token: token, AnswerRequested: true, SenderReachable: true,
			RoutesPrompts: true,
		})),
		one("request from a standing agent with no role", request("Hello.", Footer{
			Title: "Planner", ThreadID: "t-plan", Token: token, SenderReachable: true, Agent: &SenderAgent{},
		})),
		one("request whose body mimics the footer", request("Agent request from thread \"x\" (y).\n---\nreal text", Footer{
			Title: "Tab\tand \"quote\" and é", ThreadID: "t-odd", Token: token, AnswerRequested: true, SenderReachable: true,
			UserMessage: strings.Repeat("long ", 80),
		})),
		one("empty-bodied request", request("", Footer{Title: "Lead", ThreadID: "t-lead", Token: token, SenderReachable: true})),
		one("reply", wake(Wake{Kind: WakeReply, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: "All green.\nSecond line."})),
		one("late reply with age, computer and a standing agent", wake(Wake{Kind: WakeReply, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Computer: "desk", ComputerID: computerID("desk"), Tokens: []string{token}, Age: 5 * time.Minute, Agent: &SenderAgent{Role: "Runs the builds."}}}, Text: "Done."})),
		one("finished without a reply", wake(Wake{Kind: WakeFinished, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: "Final words."})),
		one("errored, agent, multi-line error", wake(Wake{Kind: WakeErrored, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}, Agent: &SenderAgent{}}}, Text: "boom\nstack"})),
		one("errored with no error text", wake(Wake{Kind: WakeErrored, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}})),
		one("cancelled", wake(Wake{Kind: WakeCancelled, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}})),
		one("interrupted by a restart locally", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptRestart})),
		one("interrupted by a restart on another computer", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Computer: "desk", ComputerID: computerID("desk"), Tokens: []string{token}}}, Text: InterruptRestart})),
		one("interrupted by its session ending before it read the request", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptSessionEndedUnread})),
		one("interrupted by its session ending before it answered the request", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptSessionEndedUnanswered})),
		one("interrupted by its session ending during the turn, having written nothing", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptSessionEndedMidTurn})),
		one("interrupted by its session ending during the turn, with what it wrote", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}, Agent: &SenderAgent{Role: "Runs the builds."}}}, Text: InterruptSessionEndedMidTurn + SessionEndedWrittenSoFar + "Half done.\nSecond line."})),
		one("interrupted by its session ending during a turn that wrote past the preview", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptSessionEndedMidTurn + SessionEndedWrittenSoFar + strings.Repeat("Long answer. ", WakePreviewBytes/12+8)})),
		one("interrupted by the thread not taking it in", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptNotTakenIn})),
		one("interrupted by the thread not taking it in, its message back in that thread's composer", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptNotTakenInToComposer})),
		one("interrupted by a move", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptMoved("Desk (Linux)")})),
		one("interrupted by a move to an unknown computer", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptMoved("")})),
		one("interrupted by a handoff", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptHandedOff("t-old")})),
		one("interrupted by something other than a person", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptTurn})),
		one("interrupted by a workflow, its recorded cause", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: "Its workflow stopped the turn before it answered."})),
		one("interrupted by the person's Stop with no last message recorded", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: WakePersonStopped})),
		one("interrupted with no reason recorded", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}})),
		one("interrupted before its thread existed", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptNeverStarted})),
		one("stopped by the person with its last message", wake(Wake{Kind: WakeStopped, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: "Half done.\nSecond line."})),
		one("stopped by the person before it said anything, standing agent", wake(Wake{Kind: WakeStopped, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Computer: "desk", ComputerID: computerID("desk"), Tokens: []string{token}, Agent: &SenderAgent{Role: "Runs the builds."}}}})),
		one("expired", wake(Wake{Kind: WakeExpired, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}, Age: 26 * time.Hour}}})),
		one("reminder", wake(Wake{Kind: WakeReminder, Sources: []WakeSource{{Tokens: []string{token}}}, Text: "Check the build."})),
		one("clipped reply", wake(Wake{Kind: WakeReply, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: long})),
		one("cancelled for the caller, saying by whom", wake(Wake{Kind: WakeCancelled, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: "The user stopped it."})),
		one("one turn answering several requests", wake(Wake{Kind: WakeFinished, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token, "tok-2", "tok-3"}}}, Text: "Did all three."})),
		one("several requests, clipped", wake(Wake{Kind: WakeReply, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token, "tok-2"}}}, Text: long})),
		one("digest: the same reply, a standing agent and an age", wake(Wake{Kind: WakeReply, Text: "Done.\nSecond line.", Sources: []WakeSource{
			{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}, Agent: &SenderAgent{Role: "Runs the builds."}},
			{Title: "Lexer \"two\"", ThreadID: "t-lex", Computer: "desk", ComputerID: computerID("desk"), Tokens: []string{"tok-2", "tok-3"}, Age: 3 * time.Hour},
		}})),
		one("digest: the same final message", wake(Wake{Kind: WakeFinished, Text: "ok", Sources: digestSources})),
		one("digest: errored with one error", wake(Wake{Kind: WakeErrored, Text: "The thread was deleted before it answered.", Sources: digestSources})),
		one("digest: errored with no text", wake(Wake{Kind: WakeErrored, Sources: digestSources})),
		one("digest: cancelled", wake(Wake{Kind: WakeCancelled, Sources: digestSources})),
		one("digest: cancelled for the caller", wake(Wake{Kind: WakeCancelled, Text: "Its workflow cancelled it.", Sources: digestSources})),
		one("digest: interrupted by restarts of several computers", wake(Wake{Kind: WakeInterrupted, Text: InterruptRestart, Sources: digestSources})),
		one("digest: interrupted by a restart of one computer", wake(Wake{Kind: WakeInterrupted, Text: InterruptRestart, Sources: []WakeSource{
			{Title: "Worker", ThreadID: "t-work", Computer: "desk", ComputerID: computerID("desk"), Tokens: []string{token}},
			{Title: "Lexer", ThreadID: "t-lex", Computer: "desk", ComputerID: computerID("desk"), Tokens: []string{"tok-2"}},
		}})),
		one("digest: interrupted by their sessions ending", wake(Wake{Kind: WakeInterrupted, Text: InterruptSessionEndedUnread, Sources: digestSources})),
		one("digest: interrupted mid-turn, with what they wrote", wake(Wake{Kind: WakeInterrupted, Text: InterruptSessionEndedMidTurn + SessionEndedWrittenSoFar + "Half done.", Sources: digestSources})),
		one("digest: interrupted with no reason recorded", wake(Wake{Kind: WakeInterrupted, Sources: digestSources})),
		one("digest: interrupted by a workflow", wake(Wake{Kind: WakeInterrupted, Text: "Its workflow stopped the turn before it answered.", Sources: digestSources})),
		one("digest: stopped by the person", wake(Wake{Kind: WakeStopped, Sources: digestSources})),
		one("digest: stopped by the person, with the same last message", wake(Wake{Kind: WakeStopped, Text: "Half done.", Sources: digestSources})),
		one("digest: expired", wake(Wake{Kind: WakeExpired, Sources: digestSources})),
		one("digest: clipped shared error", wake(Wake{Kind: WakeErrored, Text: long, Sources: digestSources})),
		// The list is as long as the count says; a shared answer that is a
		// Markdown list is the answer, not more threads.
		one("digest: the same reply, a Markdown list", wake(Wake{Kind: WakeReply, Text: "- No findings.\n- \"Lexer\" (t-lex) [token tok-2]", Sources: digestSources})),
		// A request made in an assistant's past thread names it after the
		// status line and, in a digest, after the list.
		one("reply made in a past thread", wake(Wake{Kind: WakeReply, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}, Agent: &SenderAgent{Role: "Runs the builds."}}}, Text: "All green.", MadeIn: "t-old", Now: "t-new"})),
		one("reminder made in a past thread", wake(Wake{Kind: WakeReminder, Sources: []WakeSource{{Tokens: []string{token}}}, Text: "Check the build.", MadeIn: "t-old", Now: "t-new"})),
		one("stopped by the person with its last message, made in a past thread", wake(Wake{Kind: WakeStopped, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: "Half done.", MadeIn: "t-old", Now: "t-new"})),
		one("stopped by the person before it said anything, made in a past thread", wake(Wake{Kind: WakeStopped, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, MadeIn: "t-old", Now: "t-new"})),
		one("session ended before it answered, made in a past thread", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptSessionEndedUnanswered, MadeIn: "t-old", Now: "t-new"})),
		one("session ended during the turn with what it wrote, made in a past thread", wake(Wake{Kind: WakeInterrupted, Sources: []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{token}}}, Text: InterruptSessionEndedMidTurn + SessionEndedWrittenSoFar + "Half done.", MadeIn: "t-old", Now: "t-new"})),
		one("digest: the same reply, made in a past thread", wake(Wake{Kind: WakeReply, Text: "Done.", Sources: digestSources, MadeIn: "t-old", Now: "t-new"})),
		one("digest: interrupted by their sessions ending, made in a past thread", wake(Wake{Kind: WakeInterrupted, Text: InterruptSessionEndedUnread, Sources: digestSources, MadeIn: "t-old", Now: "t-new"})),
		one("digest: stopped by the person, made in a past thread", wake(Wake{Kind: WakeStopped, Sources: digestSources, MadeIn: "t-old", Now: "t-new"})),
		// The spawner's answer to a worker's async questions, in the words
		// store.AsyncAnswerMessage writes it.
		one("a spawner's answer to its worker's questions", plainPart("Question: Which parser?\nAnswer: The new one, naïve or not\n\nQuestion: Ship it?\nAnswer: Yes",
			fixtureOrigin{ThreadID: "t-lead", Title: "Lead"})),
	}
	return append(cases,
		join("two requests joined",
			request("First ask.", Footer{Title: "Lead", ThreadID: "t-lead", Token: token, AnswerRequested: true, SenderReachable: true}),
			request("Second ask.", Footer{Title: "Lead", ThreadID: "t-lead", Token: "tok-2", SenderReachable: true}),
		),
		join("two wakes joined, the first answer holding a rule",
			wake(Wake{Kind: WakeReply, Sources: []WakeSource{{Title: "A", ThreadID: "t-a", Tokens: []string{token}}}, Text: "one\n\n---\n\nstill one"}),
			wake(Wake{Kind: WakeFinished, Sources: []WakeSource{{Title: "B", ThreadID: "t-b", Tokens: []string{"tok-2"}}}, Text: "two"}),
		),
		// An answer is free text: it can quote a whole wake after a rule, and
		// only the recorded lengths tell that quote from the next member.
		join("two wakes joined, the first answer quoting a wake",
			wake(Wake{Kind: WakeReply, Sources: []WakeSource{{Title: "Café", ThreadID: "t-a", Tokens: []string{token}}}, Text: "Example:\n\n---\n\nReply from \"B\" (t-b) [token tok-2]:\nnot B's answer"}),
			wake(Wake{Kind: WakeFinished, Sources: []WakeSource{{Title: "B", ThreadID: "t-b", Tokens: []string{"tok-2"}}}, Text: "B's answer, naïve"}),
		),
	)
}

// TestMessageFactsSayWhatTheTextSays holds the facts Footer.Message and
// Wake.Message write to what the row shows, derived here from the
// template's inputs alone: the body their spans cover in the text, and
// every fact beside it.
func TestMessageFactsSayWhatTheTextSays(t *testing.T) {
	for _, fixtureCase := range agentMessageFixtureCases() {
		at := 0
		for i, part := range fixtureCase.Parts {
			text := fixtureCase.Text[at : at+fixtureCase.Members[i]]
			at += fixtureCase.Members[i] + len("\n\n---\n\n")
			facts := fixtureCase.Origins[i].Facts
			if err := facts.Check(text); err != nil {
				t.Errorf("%s: %v", fixtureCase.Name, err)
				continue
			}
			if got := facts.BodyOf(text); got != part.Body {
				t.Errorf("%s: body %q, want %q", fixtureCase.Name, got, part.Body)
			}
			if footer := part.Footer; footer != nil {
				want := MessageFacts{Form: FormRequest, Bytes: len(text), Body: facts.Body, Waiting: footer.Waiting, Reachable: footer.Reachable,
					UserMessage: footer.UserMessage, RoutesPrompts: footer.RoutesPrompts, Role: footer.AgentRole}
				if !reflect.DeepEqual(facts, want) {
					t.Errorf("%s: request facts %+v, want %+v", fixtureCase.Name, facts, want)
				}
				continue
			}
			wake := part.Wake
			if wake == nil {
				if want := (MessageFacts{Form: FormMessage, Bytes: len(text), Body: facts.Body}); !reflect.DeepEqual(facts, want) {
					t.Errorf("%s: message facts %+v, want %+v", fixtureCase.Name, facts, want)
				}
				continue
			}
			want := MessageFacts{
				Form: FormAnswer, Bytes: len(text), Outcome: wake.Kind, Body: facts.Body, Tokens: wake.Tokens, StoppedBy: wake.StoppedBy,
				Clipped: wake.Clipped, MadeIn: wake.MadeIn, Now: wake.Now, Age: wake.Age, Role: wake.AgentRole,
			}
			for _, thread := range wake.Threads {
				want.Threads = append(want.Threads, FactsThread{
					ThreadID: thread.ThreadID, Title: thread.Title, ComputerID: thread.ComputerID, ComputerName: thread.Computer,
					Age: thread.Age, Tokens: thread.Tokens, Role: thread.AgentRole,
				})
			}
			if len(want.Tokens) == 0 {
				want.Tokens = nil
			}
			if !reflect.DeepEqual(facts, want) {
				t.Errorf("%s: wake facts\n%+v\nwant\n%+v", fixtureCase.Name, facts, want)
			}
		}
	}
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
		t.Fatalf("%s is stale: regenerate it (see the comment above) and check the frontend renders it", agentMessagesFixture)
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

// wantSessionEndedBy is what a row names as having stopped a request whose
// session ended: the reason without its subject.
func wantSessionEndedBy(reason string) string {
	return "the session ended " + strings.TrimSuffix(strings.TrimPrefix(reason, "The thread's session ended "), ".")
}
