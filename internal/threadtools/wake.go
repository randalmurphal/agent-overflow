package threadtools

import (
	"fmt"
	"strings"
	"time"
)

// A wake is what a request's caller is told of it: a status line and the
// answer, for one thread or a digest of several.

// WakePreviewBytes caps a wake body. The whole answer stays in the request
// record and thread_status returns it.
const WakePreviewBytes = 24 << 10

// AgeFloor is how old an answer has to be before a wake states its age. A
// wake that lands as soon as the answer was written says nothing about
// time, which is what "only when they apply" means.
const AgeFloor = time.Minute

// Wake kinds, one per way a request reaches its caller.
const (
	WakeReply       = "reply"
	WakeFinished    = "finished"
	WakeErrored     = "errored"
	WakeCancelled   = "cancelled"
	WakeInterrupted = "interrupted"
	WakeExpired     = "expired"
	WakeReminder    = "reminder"
)

// WakeSource is one thread a wake speaks for, with the requests of it the
// wake answers.
type WakeSource struct {
	// Title, ThreadID and Computer name the thread that answered. Computer
	// is empty when it is the caller's own. All three are empty for a
	// reminder, which no thread answers.
	Title    string
	ThreadID string
	Computer string
	// ComputerID is the id of the computer Computer names, which the text
	// never states: a digest's facts carry it so a row can open the thread.
	ComputerID string
	// Tokens are the requests this wake answers, oldest first. One turn can
	// answer several: it consumed several of the caller's messages.
	Tokens []string
	// Age is how long ago the answer was written. Stated only past
	// AgeFloor.
	Age time.Duration
	// Agent is set when this thread is a standing agent.
	Agent *SenderAgent
}

// Wake is the status line and body of a message delivered to the thread
// that made a request. It has one source unless several threads gave the
// caller the same outcome at once (a restart that interrupted them all), in
// which case one message lists them instead of one message each.
type Wake struct {
	Kind    string
	Sources []WakeSource
	// Text is the answer, the final message, the error or the note; for an
	// interruption, what the turn had written or said when its Cause carries
	// that, or the whole text of one with no known cause. It is clipped to
	// WakePreviewBytes with a pointer to thread_status.
	Text string
	// Cause is why a WakeInterrupted request was interrupted, and the one
	// thing that says who stopped it.
	Cause Cause
	// MadeIn is the assistant's past thread the requests were made in, set
	// when the wake goes to a later thread of the same assistant, Now. One
	// wake carries only requests made in the same thread.
	MadeIn string
	Now    string
}

// String renders one wake (Message).
func (w Wake) String() string {
	text, _ := w.Message()
	return text
}

// Message renders one wake, status line first, and its facts from the same
// value. A wake from a standing agent says so on the line after the status
// line, and a request made in an assistant's past thread names it on the
// line after that.
func (w Wake) Message() (string, MessageFacts) {
	facts := MessageFacts{Form: FormAnswer, Outcome: w.outcome(), MadeIn: w.MadeIn, Now: w.Now}
	for _, source := range w.Sources {
		facts.Tokens = append(facts.Tokens, source.Tokens...)
	}
	var b spanBuilder
	if len(w.Sources) > 1 {
		w.digest(&b, &facts)
		return b.done(facts)
	}
	source := w.source()
	var lines []string
	if source.Agent != nil && w.Kind != WakeReminder {
		lines = append(lines, source.Agent.sentence("That thread"))
		facts.Role = roleOf(source.Agent)
	}
	if w.Kind != WakeReminder {
		facts.Age = ageOf(source.Age)
	}
	if w.MadeIn != "" {
		lines = append(lines, WakeMadeIn(w.MadeIn, w.Now))
	}
	w.status(&b, &facts)
	if len(lines) > 0 {
		// The lines go after the status line's first line, inside an
		// answer that starts on it.
		written := b.text.String()
		at := strings.IndexByte(written, '\n')
		if at < 0 {
			at = len(written)
		}
		b.insert(at, "\n"+strings.Join(lines, "\n"))
	}
	return b.done(facts)
}

// outcome is the wake's kind as its facts name it.
func (w Wake) outcome() string {
	switch w.Kind {
	case WakeReply, WakeFinished, WakeErrored, WakeCancelled, WakeInterrupted, WakeExpired, WakeReminder:
		return w.Kind
	}
	return WakeSettled
}

// ageOf is an answer's age as a status line states it, "" under AgeFloor.
func ageOf(age time.Duration) string {
	if age < AgeFloor {
		return ""
	}
	return Age(age)
}

// WakeMadeIn names the past thread a request was made in, for a wake that
// reaches the assistant's current thread after New Thread, Same Assistant.
func WakeMadeIn(madeIn, now string) string {
	return wakeMadeInOpening + madeIn + wakeMadeInMiddle + now + "."
}

const (
	wakeMadeInOpening = "Made in thread "
	wakeMadeInMiddle  = ", before New Thread, Same Assistant; now "
)

// source is the one thread a single-source wake speaks for; a reminder has
// none.
func (w Wake) source() WakeSource {
	if len(w.Sources) == 1 {
		return w.Sources[0]
	}
	return WakeSource{}
}

// status writes a single wake's status line and its answer.
func (w Wake) status(b *spanBuilder, facts *MessageFacts) {
	source := w.source()
	who := source.who() + tokenLabel(source.Tokens)
	switch w.Kind {
	case WakeReply:
		b.write("Reply from " + who + ":\n")
		w.writeAnswer(b, facts, w.Text)
	case WakeFinished:
		b.write(who + " finished its turn without calling thread_reply. Its final message:\n")
		w.writeAnswer(b, facts, w.Text)
	case WakeErrored:
		if w.Text == "" {
			b.write(who + " errored.")
			return
		}
		b.write(who + " errored: ")
		w.writeAnswer(b, facts, w.Text)
	case WakeCancelled:
		if w.Text == "" {
			b.write(who + " was cancelled.")
			return
		}
		b.write(who + " was cancelled: ")
		w.writeAnswer(b, facts, w.Text)
	case WakeInterrupted:
		// The cause the settlement recorded, in its words. A restart names
		// the computer that restarted.
		if w.Cause.Code == CauseRestart {
			facts.StoppedBy = "a restart of " + restartedComputer(source.Computer)
			b.write(fmt.Sprintf("%s was interrupted by a restart of %s.", who, restartedComputer(source.Computer)))
			return
		}
		// The reason is said whole; with no known cause the text is the
		// answer, whole, and names no stopper.
		shown := w.Cause.render(w.Text)
		facts.StoppedBy = shown.stoppedBy
		b.write(who + ": ")
		w.writeShown(b, facts, shown)
	case WakeExpired:
		b.write(who + " did not answer within a day.")
	case WakeReminder:
		b.write("Reminder you set" + tokenLabel(source.Tokens) + ":\n")
		w.writeAnswer(b, facts, w.Text)
	default:
		b.write(who + " settled.")
	}
}

// writeAnswer writes text as the message's body, clipped to the preview,
// with the pointer to the rest after it.
func (w Wake) writeAnswer(b *spanBuilder, facts *MessageFacts, text string) {
	clipped, cut := text, len(text) > WakePreviewBytes
	if cut {
		clipped = clipBytes(text, WakePreviewBytes)
		facts.Clipped = true
	}
	b.writeBody(clipped)
	if cut {
		b.write("\n" + WakePreviewPointer(w.firstToken()))
	}
}

// writeShown writes a rendered cause: its reason and its own sentence
// whole, and only the answer clipped to the preview.
func (w Wake) writeShown(b *spanBuilder, facts *MessageFacts, shown shownCause) {
	b.write(shown.reason)
	b.writeBody(shown.sentence)
	w.writeAnswer(b, facts, shown.answer)
}

// digest renders a wake that speaks for several threads: what happened to
// each of them, one line naming each with its tokens (and, under it, that it
// is a standing agent when it is one), then the text they share. The text is
// last so a long one does not bury the list, and the first line says it is
// the same for each thread, so a cause written about one thread reads as it
// was written.
func (w Wake) digest(b *spanBuilder, facts *MessageFacts) {
	count := len(w.Sources)
	var head string
	// shown is what follows the list when they share it: the text, or a
	// shared cause in its words.
	shown := shownCause{answer: w.Text}
	shared := w.Text != ""
	switch w.Kind {
	case WakeReply:
		head, shared = fmt.Sprintf("%d threads sent the same reply:", count), true
	case WakeFinished:
		head, shared = fmt.Sprintf("%d threads finished their turns without calling thread_reply, each with the same final message:", count), true
	case WakeErrored:
		head = fmt.Sprintf("%d threads errored.", count)
		if shared {
			head = fmt.Sprintf("%d threads errored, each with the same error:", count)
		}
	case WakeCancelled:
		head = fmt.Sprintf("%d threads were cancelled.", count)
		if shared {
			head = fmt.Sprintf("%d threads were cancelled, each for the same reason:", count)
		}
	case WakeInterrupted:
		if w.Cause.Code == CauseRestart {
			head, shared = fmt.Sprintf("%d threads were interrupted by a restart of %s.", count, w.restartedComputers()), false
			facts.StoppedBy = "a restart of " + w.restartedComputers()
			break
		}
		// Their shared cause in its words, or with no known cause their
		// shared text, whole.
		cause := w.Cause.render(w.Text)
		facts.StoppedBy = cause.stoppedBy
		switch {
		case w.Cause.Code == CauseUserStop:
			// The head says it for every thread.
			head, shown, shared = fmt.Sprintf("%d threads: %s", count, WakeDigestPersonStopped), shownCause{answer: cause.answer}, cause.answer != ""
			if shared {
				head += " Each one's last message:"
			}
		default:
			head, shown = fmt.Sprintf("%d threads were interrupted, each for the same reason:", count), cause
			shared = cause.reason+cause.sentence+cause.answer != ""
		}
	case WakeExpired:
		head, shared = fmt.Sprintf("%d threads did not answer within a day.", count), false
	default:
		head, shared = fmt.Sprintf("%d threads settled.", count), false
	}
	b.write(head)
	facts.Threads = make([]FactsThread, 0, count)
	for _, source := range w.Sources {
		b.write("\n- " + source.who() + tokenLabel(source.Tokens))
		if source.Agent != nil {
			b.write("\n  " + source.Agent.sentence("That thread"))
		}
		facts.Threads = append(facts.Threads, FactsThread{
			ThreadID: source.ThreadID, Title: source.Title, ComputerID: source.ComputerID, ComputerName: source.Computer,
			Age: ageOf(source.Age), Tokens: source.Tokens, Role: roleOf(source.Agent),
		})
	}
	if w.MadeIn != "" {
		b.write("\n" + WakeMadeIn(w.MadeIn, w.Now))
	}
	if shared {
		b.write("\n")
		w.writeShown(b, facts, shown)
	}
}

// restartedComputers names the computer a restart stopped a digest's
// requests on: the one they share, or each one's when they differ.
func (w Wake) restartedComputers() string {
	for _, source := range w.Sources[1:] {
		if source.Computer != w.Sources[0].Computer {
			return "the computer each ran on"
		}
	}
	return restartedComputer(w.Sources[0].Computer)
}

func (s WakeSource) who() string {
	return fmt.Sprintf("%q (%s%s%s)", s.Title, s.ThreadID, onComputer(s.Computer), answered(s.Age))
}

// tokenLabel names the requests a wake answers for one thread.
func tokenLabel(tokens []string) string {
	switch len(tokens) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" [token %s]", tokens[0])
	default:
		return fmt.Sprintf(" [tokens %s]", strings.Join(tokens, ", "))
	}
}

// firstToken is the request a clipped body points thread_status at.
func (w Wake) firstToken() string {
	for _, source := range w.Sources {
		if len(source.Tokens) > 0 {
			return source.Tokens[0]
		}
	}
	return ""
}

// WakePreviewPointer is the notice a clipped wake body ends with.
func WakePreviewPointer(token string) string {
	return fmt.Sprintf("[preview; thread_status %s for the whole answer]", token)
}

func answered(age time.Duration) string {
	if age := ageOf(age); age != "" {
		return ", answered " + age + " ago"
	}
	return ""
}

// restartedComputer names the computer a restart stopped a request on:
// the answering thread's, which is the caller's own when name is empty.
func restartedComputer(name string) string {
	if name == "" {
		return "this computer"
	}
	return name
}
