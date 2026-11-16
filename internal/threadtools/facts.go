package threadtools

import (
	"slices"
	"strings"
)

// MessageFacts is what a message the app writes for an agent says, as the
// fields a person's view renders: the timeline row reads these, never the
// text, which is written for the agent. Each message's text and facts come
// from one value through one function (Footer.Message, Wake.Message,
// usermessage.WakeMessage), so the two cannot disagree. The row carries
// them on its attribution (usermessage.OriginThread.Facts,
// usermessage.OriginWake.Facts).
type MessageFacts struct {
	// Outcome is what an answer says happened to the requests it answers:
	// a WakeReply, WakeFinished, WakeErrored, WakeCancelled,
	// WakeInterrupted (a person's Stop included, told apart by StoppedBy),
	// WakeExpired or WakeReminder, WakeSettled for a settlement none of
	// those names, or OutcomeWorkerStopped for the notice that the person
	// stopped a worker after it answered. Empty on every other message.
	Outcome string `json:"outcome,omitempty"`
	// Body is where the message's own words are in its text: what the
	// sender wrote, the answer, or the data a wake delivers. The spans are
	// UTF-8 byte offsets, read in order and joined as they are; most bodies
	// are one span. Empty when the message has none.
	Body []Span `json:"body,omitempty"`
	// Headline is what the row's header says the message is about, when
	// that is not the first line of Body: a loop's note, a watch's first
	// output line, a remote job's result.
	Headline string `json:"headline,omitempty"`

	// Waiting, Reachable, UserMessage and RoutesPrompts are a request's
	// footer: the sender waits for the answer, this computer can reach the
	// sender's, the sender thread's latest message from the person as the
	// footer quotes it, and the sender spawned the receiver and answers its
	// prompts.
	Waiting       bool   `json:"waiting,omitempty"`
	Reachable     bool   `json:"reachable,omitempty"`
	UserMessage   string `json:"userMessage,omitempty"`
	RoutesPrompts bool   `json:"routesPrompts,omitempty"`
	// Role is the sender's or the answering thread's role line when it is a
	// standing agent, "" for one with none; nil when it is not one.
	Role *string `json:"role,omitempty"`

	// Tokens are every request an answer answers, oldest first.
	Tokens []string `json:"tokens,omitempty"`
	// Age is how long before the answer was delivered it was written, as
	// the status line states it (Age); empty under AgeFloor.
	Age string `json:"age,omitempty"`
	// StoppedBy is what stopped an interrupted request, in words: a restart
	// of a computer, the user, the session ending, a move or an interrupt.
	// Empty when the reason names no stopper, and the reason is the body.
	StoppedBy string `json:"stoppedBy,omitempty"`
	// Clipped is set when the answer was cut to the preview, so
	// thread_status with the first token returns the rest.
	Clipped bool `json:"clipped,omitempty"`
	// MadeIn and Now are the assistant's past thread the requests were made
	// in and its current thread, when the answer reached the current one
	// after New Thread, Same Assistant.
	MadeIn string `json:"madeIn,omitempty"`
	Now    string `json:"now,omitempty"`
	// Threads are the threads a digest speaks for, two or more; the
	// attribution names the first, and a single answer's thread is the
	// attribution itself.
	Threads []FactsThread `json:"threads,omitempty"`
}

// FactsThread is one thread a digest answer speaks for, as its row names it
// and opens it.
type FactsThread struct {
	ThreadID string `json:"threadId"`
	Title    string `json:"title,omitempty"`
	// ComputerID and ComputerName name the thread's computer, empty for the
	// caller's own: the id a row opens it through, the name it reads as.
	ComputerID   string   `json:"computerId,omitempty"`
	ComputerName string   `json:"computerName,omitempty"`
	Age          string   `json:"age,omitempty"`
	Tokens       []string `json:"tokens,omitempty"`
	Role         *string  `json:"role,omitempty"`
}

// Span is a range of a message's text in UTF-8 bytes, End exclusive.
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Outcomes a MessageFacts names beyond the wake kinds.
const (
	// WakeSettled is a wake whose settlement no other outcome names.
	WakeSettled = "settled"
	// OutcomeWorkerStopped is the notice telling a spawner the person
	// stopped its worker after the spawn had answered.
	OutcomeWorkerStopped = "worker-stopped"
)

// SpanText is the text spans cover in text, joined as they are. A span
// past the text's end is cut at it, so a message whose trailing white
// space a requeue trimmed still reads; one that does not fit at all reads
// as nothing.
func SpanText(text string, spans []Span) string {
	var out strings.Builder
	for _, span := range spans {
		start, end := min(max(span.Start, 0), len(text)), min(max(span.End, 0), len(text))
		if start < end {
			out.WriteString(text[start:end])
		}
	}
	return out.String()
}

// spanBuilder writes a message's text and records where its body is.
type spanBuilder struct {
	text strings.Builder
	body []Span
}

func (b *spanBuilder) write(text string) { b.text.WriteString(text) }

// writeBody writes text that is the message's body.
func (b *spanBuilder) writeBody(text string) {
	if text == "" {
		return
	}
	start := b.text.Len()
	b.text.WriteString(text)
	b.body = append(b.body, Span{Start: start, End: b.text.Len()})
}

// insert puts text at offset at of what is written, moving the body spans
// after it and splitting the one it falls inside, so they still cover the
// same words.
func (b *spanBuilder) insert(at int, text string) {
	written := b.text.String()
	b.text.Reset()
	b.text.WriteString(written[:at] + text + written[at:])
	var spans []Span
	for _, span := range b.body {
		switch {
		case span.End <= at:
			spans = append(spans, span)
		case span.Start >= at:
			spans = append(spans, Span{Start: span.Start + len(text), End: span.End + len(text)})
		default:
			spans = append(spans, Span{Start: span.Start, End: at}, Span{Start: at + len(text), End: span.End + len(text)})
		}
	}
	b.body = spans
}

// roleOf is a standing agent's role as facts carry it.
func roleOf(agent *SenderAgent) *string {
	if agent == nil {
		return nil
	}
	role := agent.Role
	return &role
}

// interruptStopper reads the reason an interrupted request records
// (Interrupt*): what stopped it, in words, and where in reason the part a
// person reads as the message starts, -1 for none. A reason that names no
// stopper (a thread's cancel or a workflow, worded by the stop; a request
// whose thread never existed) is the message itself.
func interruptStopper(reason string) (stoppedBy string, shownFrom int) {
	switch {
	case reason == "" || reason == InterruptTurn:
		return "an interrupt", -1
	case strings.HasPrefix(reason, InterruptSessionEndedMidTurn+SessionEndedWrittenSoFar):
		return sessionEndedBy(InterruptSessionEndedMidTurn), len(InterruptSessionEndedMidTurn + SessionEndedWrittenSoFar)
	case slices.Contains(InterruptSessionEndedReasons, reason):
		return sessionEndedBy(reason), -1
	case reason == InterruptNotTakenIn:
		return "the thread did not take it in", -1
	case reason == InterruptNotTakenInToComposer:
		return "the thread did not take it in, and its message went back to that thread's composer", -1
	case reason == WakePersonStopped:
		return "the user", -1
	case strings.HasPrefix(reason, InterruptHandedOffSentence+" "):
		// The rest names the previous thread, for the sender to read.
		return "the assistant moving to a new thread", len(InterruptHandedOffSentence + " ")
	}
	if destination, moved := strings.CutPrefix(reason, interruptMovedOpening); moved && strings.HasSuffix(destination, interruptMovedClosing) {
		return "a move to " + strings.TrimSuffix(destination, interruptMovedClosing), -1
	}
	return "", 0
}

// sessionEndedBy names a session's end as what stopped a request: the
// reason without its subject.
func sessionEndedBy(reason string) string {
	return "the session ended " + strings.TrimSuffix(strings.TrimPrefix(reason, "The thread's session ended "), ".")
}
