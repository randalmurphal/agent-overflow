package usermessage

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// AgentMessage is a message the app writes into a thread: its text, and the
// attribution and facts its row is read by. Each constructor takes the value
// the text is rendered from and derives all three from it, so the sender the
// text names is the sender the attribution names, and the facts describe the
// text they are stored with:
//
//   - Request: a thread's request, from its footer (threadtools.Footer).
//   - Answer: what a request's caller is told of it (threadtools.Wake).
//   - StoppedWorker: the notice that the user stopped a spawner's worker.
//   - Message: a thread's message with no footer, from its sender and body.
//   - Report: every other message the app writes for an agent, of one of
//     the ReportKinds.
//   - PersonMessage and AppMessage: the person's own words and the app's
//     on their behalf, which no agent row reads.
//   - DecodeStored: a message read back from where it was stored, checked.
//
// A message is a value: it shares nothing with the inputs it was built from,
// and Attribution returns a copy, so nothing a caller does to either changes
// the message.
type AgentMessage struct {
	text   string
	origin string
	thread *OriginThread
	wake   *OriginWake
}

// Attribution is a message's attribution as a row's meta stores it: its
// origin and the OriginThread or OriginWake, with the facts, of that origin.
type Attribution struct {
	Origin string
	Thread *OriginThread
	Wake   *OriginWake
}

// Text is what the message says, to the agent and in the row.
func (m AgentMessage) Text() string { return m.text }

// Origin is the message's Origin, the row meta field of the same name.
func (m AgentMessage) Origin() string { return m.origin }

// Attribution is a copy of the message's attribution, which shares nothing
// with the message.
func (m AgentMessage) Attribution() Attribution {
	return Attribution{Origin: m.origin, Thread: m.thread.clone(), Wake: m.wake.clone()}
}

func (t *OriginThread) clone() *OriginThread {
	if t == nil {
		return nil
	}
	copied := *t
	copied.Facts = cloneFacts(t.Facts)
	return &copied
}

func (w *OriginWake) clone() *OriginWake {
	if w == nil {
		return nil
	}
	copied := *w
	copied.Facts = cloneFacts(w.Facts)
	if w.Report != nil {
		report := *w.Report
		copied.Report = &report
	}
	return &copied
}

func cloneFacts(facts *threadtools.MessageFacts) *threadtools.MessageFacts {
	if facts == nil {
		return nil
	}
	copied := facts.Clone()
	return &copied
}

// requestKinds are the OriginThread kinds of a request.
var requestKinds = []string{store.ThreadRequestSpawn, store.ThreadRequestSend, store.ThreadRequestAsk}

// Request is a thread's request of kind (store.ThreadRequestSpawn, Send or
// Ask) carrying body, rendered with footer (threadtools.Footer.Message) and
// attributed to from under the footer's token. from is the thread the
// footer names: a sender that is not, or no thread at all, is refused, as
// is a footer with no token to answer by. Its computer is the attribution's
// alone (the footer names only another computer, and in this computer's
// words for it), so a row opens the sender where it is.
func Request(kind string, from Sender, footer threadtools.Footer, body string) (AgentMessage, error) {
	if !slices.Contains(requestKinds, kind) {
		return AgentMessage{}, fmt.Errorf("a request of kind %q: the kinds are %s", kind, strings.Join(requestKinds, ", "))
	}
	if from.ThreadID != footer.ThreadID || from.Title != footer.Title {
		return AgentMessage{}, fmt.Errorf("a request from thread %q (%s) under a footer naming thread %q (%s)",
			from.Title, from.ThreadID, footer.Title, footer.ThreadID)
	}
	if from.ThreadID == "" {
		return AgentMessage{}, errors.New("a request from no thread")
	}
	if footer.Token == "" {
		return AgentMessage{}, fmt.Errorf("a request from thread %s with no token", from.ThreadID)
	}
	text, facts := footer.Message(body)
	return threadMessage(text, facts, OriginThread{
		ComputerID: from.ComputerID, ComputerName: from.ComputerName, ThreadID: from.ThreadID, Title: from.Title,
		Token: footer.Token, Kind: kind,
	}), nil
}

// threadMessage is an agent thread's message of text, attributed to from
// with facts. The facts are copied: what a renderer returns can share slices
// with the value it rendered, which stays its caller's.
func threadMessage(text string, facts threadtools.MessageFacts, from OriginThread) AgentMessage {
	facts = facts.Clone()
	from.Facts = &facts
	return AgentMessage{text: text, origin: OriginAgentThread, thread: &from}
}

// Answer is what a request's caller is told of it (threadtools.Wake.Message),
// attributed to the first thread it speaks for under that thread's first
// token: the request whose send id it is queued under. A wake that speaks
// for several threads names them all in its facts. A reminder is the
// caller's own: its one source names the reminders it answers and no
// thread, and it is attributed to none. A wake that answers no request, or
// a reminder that names a thread, is refused.
func Answer(wake threadtools.Wake) (AgentMessage, error) {
	if len(wake.Sources) == 0 {
		return AgentMessage{}, errors.New("an answer to no request")
	}
	for _, source := range wake.Sources {
		if len(source.Tokens) == 0 {
			return AgentMessage{}, fmt.Errorf("an answer from thread %q (%s) to none of its requests", source.Title, source.ThreadID)
		}
	}
	first := wake.Sources[0]
	from := OriginThread{Kind: OriginKindWake, Token: first.Tokens[0]}
	if wake.Kind == threadtools.WakeReminder {
		if len(wake.Sources) > 1 || first.ThreadID != "" || first.Title != "" || first.ComputerID != "" || first.Computer != "" {
			return AgentMessage{}, fmt.Errorf("a reminder from thread %q (%s): a reminder is the caller's own", first.Title, first.ThreadID)
		}
	} else {
		from.ComputerID, from.ComputerName, from.ThreadID, from.Title = first.ComputerID, first.Computer, first.ThreadID, first.Title
	}
	text, facts := wake.Message()
	return threadMessage(text, facts, from), nil
}

// StoppedWorker is the notice telling a spawner the user stopped its worker
// after the spawn had answered (threadtools.WorkerStopped), attributed to the
// worker under the spawn's token, as the spawn's answer was. computerID and
// computerName name the worker's computer, empty for the spawner's own. A
// notice that names no worker or no token is refused.
func StoppedWorker(notice threadtools.WorkerStopped, computerID, computerName string) (AgentMessage, error) {
	if notice.ThreadID == "" || notice.Token == "" {
		return AgentMessage{}, fmt.Errorf("a stopped worker's notice for thread %q under token %q", notice.ThreadID, notice.Token)
	}
	text, facts := notice.Message()
	return threadMessage(text, facts, OriginThread{
		ComputerID: computerID, ComputerName: computerName, ThreadID: notice.ThreadID, Title: notice.Title,
		Token: notice.Token, Kind: OriginKindWake,
	}), nil
}

// Sender is the thread a Message is from.
type Sender struct {
	ComputerID   string
	ComputerName string
	ThreadID     string
	Title        string
}

// Message is a thread's message that is no request and no answer to one of
// the receiver's (threadtools.PlainMessage), such as a spawner's answer to
// its worker's questions: body as written, attributed to from. A message
// from no thread is refused.
func Message(from Sender, body string) (AgentMessage, error) {
	if from.ThreadID == "" {
		return AgentMessage{}, errors.New("a thread's message from no thread")
	}
	text, facts := threadtools.PlainMessage(body)
	return threadMessage(text, facts, OriginThread{
		ComputerID: from.ComputerID, ComputerName: from.ComputerName, ThreadID: from.ThreadID, Title: from.Title,
	}), nil
}

// PersonMessage is the person's own words, which carry no attribution.
func PersonMessage(text string) AgentMessage { return AgentMessage{text: text} }

// AppMessage is Agent Overflow writing in its own voice on the person's
// behalf, such as the Continue after a usage limit (OriginApp): the
// person's row, labelled as the app's, with nothing an agent row reads.
func AppMessage(text string) AgentMessage { return AgentMessage{text: text, origin: OriginApp} }

// ErrStoredMessage is what DecodeStored returns for a stored message whose
// attribution or facts are not what any constructor writes.
var ErrStoredMessage = errors.New("the stored message's attribution does not hold")

// DecodeStored is a message read back from where it was stored, a queue
// row's payload or a joined row's member, with the attribution it was
// written with. It is checked as it is read: an attribution of the wrong
// origin, or facts that do not describe text (threadtools.MessageFacts.Check),
// are refused with an error wrapping ErrStoredMessage. The message returned
// with that error still carries text, to be delivered as written, and its
// sender, with facts that claim nothing of it (threadtools.Whole). An agent
// thread's message with no facts at all is one main wrote, kept as it is
// for Describe's legacy reader.
func DecodeStored(text, origin string, thread *OriginThread, wake *OriginWake) (AgentMessage, error) {
	message := AgentMessage{text: text, origin: origin, thread: thread.clone(), wake: wake.clone()}
	refuse := func(format string, args ...any) (AgentMessage, error) {
		err := fmt.Errorf("%w: %s", ErrStoredMessage, fmt.Sprintf(format, args...))
		whole := threadtools.Whole(text)
		switch {
		case message.thread != nil:
			message.thread.Facts = &whole
		case message.wake != nil:
			message.wake.Facts = &whole
		}
		return message, err
	}
	switch origin {
	case "", OriginApp:
		if thread != nil || wake != nil {
			message.thread, message.wake = nil, nil
			return message, fmt.Errorf("%w: a message of origin %q carries an agent's attribution", ErrStoredMessage, origin)
		}
		return message, nil
	case OriginAgentThread:
		if wake != nil {
			message.wake = nil
			return refuse("an agent thread's message carries a wake's attribution")
		}
		if thread == nil || thread.Facts == nil {
			return message, nil
		}
		if err := thread.Facts.Check(text); err != nil {
			return refuse("%v", err)
		}
		return message, nil
	case OriginAgentWake:
		if thread != nil {
			message.thread = nil
		}
		if wake == nil {
			return message, fmt.Errorf("%w: a report names no reporter", ErrStoredMessage)
		}
		if !ReportKind(wake.Kind).known() {
			return refuse("a report of kind %q", wake.Kind)
		}
		if thread != nil {
			return refuse("a report carries a thread's attribution")
		}
		if wake.Facts == nil {
			return refuse("a report has no facts")
		}
		if err := wake.Facts.Check(text); err != nil {
			return refuse("%v", err)
		}
		return message, nil
	}
	message.thread, message.wake = nil, nil
	return message, fmt.Errorf("%w: origin %q is no message the app writes", ErrStoredMessage, origin)
}
