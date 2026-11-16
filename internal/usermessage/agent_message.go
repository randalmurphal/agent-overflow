package usermessage

import (
	"strings"

	"agent-overflow/internal/stringsx"
	"agent-overflow/internal/threadtools"
)

// AgentMessage is a message the app writes into a thread: its text and the
// attribution its row is read by, with the facts of what it says. The
// constructors are the only way to make one, and each takes the facts from
// the value the text was rendered from, so no row carries text its facts
// do not describe:
//
//   - ThreadMessage: a thread request or its answer, rendered by
//     threadtools (Footer.Message, Wake.Message).
//   - WakeMessage: every other message the app writes for an agent, the
//     one constructor a new kind of such message uses.
//   - PersonMessage: the person's own words the app sends for them.
//   - StoredMessage: one read back from the queue or a joined row, with
//     the attribution it was written with.
type AgentMessage struct {
	text   string
	origin string
	thread *OriginThread
	wake   *OriginWake
}

// Text is what the message says, to the agent and in the row.
func (m AgentMessage) Text() string { return m.text }

// Origin, OriginThread and OriginWake are the message's attribution, the
// row meta fields of the same names.
func (m AgentMessage) Origin() string              { return m.origin }
func (m AgentMessage) OriginThread() *OriginThread { return m.thread }
func (m AgentMessage) OriginWake() *OriginWake     { return m.wake }

// ThreadMessage is a request from another thread, or the answer a request
// gets back, attributed to from: text and facts as one threadtools render
// returned them.
func ThreadMessage(from OriginThread, text string, facts threadtools.MessageFacts) AgentMessage {
	from.Facts = &facts
	return AgentMessage{text: text, origin: OriginAgentThread, thread: &from}
}

// HeadlineBytes bounds the header line a WakeMessage's facts carry.
const HeadlineBytes = 512

// WakeMessage is a message the app writes for an agent that is no thread
// request or answer: a loop's fire, a watch's output, an outside send, a
// worker's prompt, a remote job's report, a workflow run's wake, an
// assistant's opening. Its text is notice, the line written for the agent
// that the row does not show, then data, which the row shows; a message
// that is prose throughout has no notice. Blank lines that open data are
// spacing, not body. headline is what the row's header says it is about,
// folded onto one bounded line; empty when that is the data's first line.
func WakeMessage(from OriginWake, notice, data, headline string) AgentMessage {
	text, start := data, 0
	if notice != "" {
		text, start = notice+"\n"+data, len(notice)+1
	}
	start += len(data) - len(strings.TrimLeft(data, "\n"))
	facts := threadtools.MessageFacts{Headline: stringsx.ClipRunes(strings.Join(strings.Fields(headline), " "), HeadlineBytes)}
	if start < len(text) {
		facts.Body = []threadtools.Span{{Start: start, End: len(text)}}
	}
	from.Facts = &facts
	return AgentMessage{text: text, origin: OriginAgentWake, wake: &from}
}

// PersonMessage is the person's own words, which carry no attribution.
func PersonMessage(text string) AgentMessage { return AgentMessage{text: text} }

// AppMessage is Agent Overflow writing in its own voice on the person's
// behalf, such as the Continue after a usage limit (OriginApp): the
// person's row, labelled as the app's, with nothing an agent row reads.
func AppMessage(text string) AgentMessage { return AgentMessage{text: text, origin: OriginApp} }

// StoredMessage is a message read back from where it was stored, a queue
// row's payload or a joined row's part, with the attribution it was written
// with, facts included.
func StoredMessage(text, origin string, thread *OriginThread, wake *OriginWake) AgentMessage {
	return AgentMessage{text: text, origin: origin, thread: thread, wake: wake}
}
