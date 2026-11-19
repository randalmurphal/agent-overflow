package threadtools

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// MessageFacts is what a message the app writes for an agent says, as the
// fields a person's view renders: the timeline row reads these, never the
// text, which is written for the agent. Each message's text and facts come
// from one value through one function (Footer.Message, Wake.Message,
// WorkerStopped.Message and the usermessage constructors), so the two cannot
// disagree. The row carries them on its attribution
// (agentmeta.OriginThread.Facts, agentmeta.OriginWake.Facts), and a
// reader takes them only through agentmeta.Describe, which checks them
// against the text (Check).
type MessageFacts struct {
	// Form is what the message is: FormRequest, FormAnswer, FormMessage,
	// FormReport, or FormWhole for one nothing is known of but its text.
	Form string `json:"form"`
	// Bytes is the length in bytes of the text the facts describe. Facts
	// whose text changed behind their back no longer describe it, and a
	// reader shows that message whole (Check).
	Bytes int `json:"bytes"`
	// Outcome is what an answer says happened to the requests it answers:
	// a WakeReply, WakeFinished, WakeErrored, WakeCancelled,
	// WakeInterrupted (a person's Stop included, told apart by StoppedBy),
	// WakeExpired or WakeReminder, WakeSettled for a settlement none of
	// those names, or OutcomeWorkerStopped for the notice that the person
	// stopped a worker after it answered. Set on an answer only.
	Outcome string `json:"outcome,omitempty"`
	// Body is where the message's own words are in its text: what the
	// sender wrote, the answer, or the data a wake delivers. The spans are
	// UTF-8 byte offsets on character boundaries, in order, not empty and
	// not overlapping, read in order and joined as they are; most bodies
	// are one span. Empty when the message has none.
	Body []Span `json:"body,omitempty"`
	// Headline is what the row's header says the message is about. A
	// writer sets it when that is not the first line of Body (a loop's
	// note, a watch's first output line, a remote job's result);
	// agentmeta.Describe sets it otherwise.
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

// The forms of message MessageFacts describe.
const (
	// FormRequest is a thread's request with its footer (Footer.Message).
	FormRequest = "request"
	// FormAnswer is what a request's caller is told of it (Wake.Message,
	// WorkerStopped.Message).
	FormAnswer = "answer"
	// FormMessage is a thread's message with no footer, all of whose text
	// is what it says (usermessage.Message).
	FormMessage = "message"
	// FormReport is a report the app writes for an agent: a loop's fire, a
	// watch's output, a worker's prompt, a remote job's completion
	// (usermessage.Report).
	FormReport = "report"
	// FormWhole is a message nothing is known of but its text and who sent
	// it, all of which is its body: one whose facts are missing or do not
	// describe its text (Whole).
	FormWhole = "whole"
)

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

// outcomes are the Outcome values an answer can have.
var outcomes = []string{
	WakeReply, WakeFinished, WakeErrored, WakeCancelled, WakeInterrupted, WakeExpired, WakeReminder,
	WakeSettled, OutcomeWorkerStopped,
}

// ErrFactsMismatch is what Check returns for facts that do not describe the
// text they are stored with.
var ErrFactsMismatch = errors.New("the message's facts do not describe its text")

// Check reports whether f describes text: written for text of its length,
// of a known form, with an outcome on an answer and on nothing else, and
// with body spans inside text, in order, not empty, not overlapping and on
// character boundaries. A reader trusts the spans of facts that pass, and
// of no others.
func (f MessageFacts) Check(text string) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrFactsMismatch, fmt.Sprintf(format, args...))
	}
	if f.Bytes != len(text) {
		return fail("written for %d bytes, the text has %d", f.Bytes, len(text))
	}
	switch f.Form {
	case FormRequest, FormMessage, FormReport, FormWhole:
		if f.Outcome != "" {
			return fail("a %s has no outcome, this one says %q", f.Form, f.Outcome)
		}
	case FormAnswer:
		if !slices.Contains(outcomes, f.Outcome) {
			return fail("an answer's outcome %q is none known", f.Outcome)
		}
	default:
		return fail("form %q is none known", f.Form)
	}
	at := 0
	for _, span := range f.Body {
		if span.Start < at || span.End <= span.Start || span.End > len(text) {
			return fail("body span [%d,%d) is out of order, empty or past the text", span.Start, span.End)
		}
		if !utf8.RuneStart(text[span.Start]) || span.End < len(text) && !utf8.RuneStart(text[span.End]) {
			return fail("body span [%d,%d) cuts a character", span.Start, span.End)
		}
		at = span.End
	}
	if f.Form == FormWhole && !slices.Equal(f.Body, wholeBody(text)) {
		return fail("a whole message's body is all of its text")
	}
	return nil
}

// BodyOf is the text f's body spans cover in text, joined as they are. f
// describes text (Check).
func (f MessageFacts) BodyOf(text string) string {
	if len(f.Body) == 1 {
		return text[f.Body[0].Start:f.Body[0].End]
	}
	var out strings.Builder
	for _, span := range f.Body {
		out.WriteString(text[span.Start:span.End])
	}
	return out.String()
}

// Whole is the facts of a message nothing is known of but its text, all
// of which is its body (FormWhole).
func Whole(text string) MessageFacts {
	return MessageFacts{Form: FormWhole, Bytes: len(text), Body: wholeBody(text)}
}

func wholeBody(text string) []Span {
	if text == "" {
		return nil
	}
	return []Span{{Start: 0, End: len(text)}}
}

// Clone is a copy of f that shares nothing with it.
func (f MessageFacts) Clone() MessageFacts {
	f.Body = slices.Clone(f.Body)
	f.Tokens = slices.Clone(f.Tokens)
	f.Role = cloneRole(f.Role)
	if f.Threads != nil {
		threads := make([]FactsThread, len(f.Threads))
		for i, thread := range f.Threads {
			thread.Tokens = slices.Clone(thread.Tokens)
			thread.Role = cloneRole(thread.Role)
			threads[i] = thread
		}
		f.Threads = threads
	}
	return f
}

func cloneRole(role *string) *string {
	if role == nil {
		return nil
	}
	value := *role
	return &value
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

// done is what was written and the facts of it: facts with the body the
// builder recorded and the length of the text.
func (b *spanBuilder) done(facts MessageFacts) (string, MessageFacts) {
	text := b.text.String()
	facts.Body, facts.Bytes = b.body, len(text)
	return text, facts
}

// roleOf is a standing agent's role as facts carry it.
func roleOf(agent *SenderAgent) *string {
	if agent == nil {
		return nil
	}
	role := agent.Role
	return &role
}

// PlainMessage renders a thread's message that carries no footer and
// answers no request of the receiver's, such as a spawner's answer to its
// worker's questions: its text is body as written, and all its words are
// its body.
func PlainMessage(body string) (string, MessageFacts) {
	words := strings.TrimSpace(body)
	lead := strings.Index(body, words)
	var b spanBuilder
	b.write(body[:lead])
	b.writeBody(words)
	b.write(body[lead+len(words):])
	return b.done(MessageFacts{Form: FormMessage})
}

// WorkerStopped is the notice telling a spawner the user stopped the worker
// it spawned after the spawn had answered: the worker, the spawn's token, and
// each part the Stop could not bring to rest, none when it is at rest.
type WorkerStopped struct {
	Title    string
	ThreadID string
	Token    string
	Failures []string
}

// Message renders the notice and its facts, from the same value. All of its
// text is its body.
func (w WorkerStopped) Message() (string, MessageFacts) {
	state := "It is at rest, with nothing running or queued; nothing it did was undone."
	if len(w.Failures) > 0 {
		state = "These parts of the stop failed and may still be running or queued: " +
			strings.TrimSuffix(strings.Join(w.Failures, "; "), ".") + "."
	}
	var b spanBuilder
	b.writeBody(fmt.Sprintf("The user stopped thread %q (%s), which you spawned, from the Agents tray after it had answered you; you did not stop it. %s A later message starts it again.",
		w.Title, w.ThreadID, state))
	return b.done(MessageFacts{
		Form: FormAnswer, Outcome: OutcomeWorkerStopped, Tokens: []string{w.Token}, StoppedBy: "the user",
	})
}
