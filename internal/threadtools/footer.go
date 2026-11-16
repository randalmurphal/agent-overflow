package threadtools

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// The fixed templates both ends read. A footer says where a message came
// from and what to do about it; a wake header says what happened to a
// request. Nothing else belongs in either.
//
// The wrapping in the plan's templates is that document's; a message
// carries one logical line per sentence group. `on <computer>` and
// `answered <age> ago` appear only when they apply, and the lines offering
// thread_show on the sender's thread and thread_send back appear only when
// the receiver can reach the sender's computer, since pairing is
// directional and the receiver may hold no credential for it.

// UserMessageQuoteRunes caps the quoted line of the sender's latest user
// message.
const UserMessageQuoteRunes = 300

// WakePreviewBytes caps a wake body. The whole answer stays in the request
// record and thread_status returns it.
const WakePreviewBytes = 24 << 10

// AgeFloor is how old an answer has to be before a wake states its age. A
// wake that lands as soon as the answer was written says nothing about
// time, which is what "only when they apply" means.
const AgeFloor = time.Minute

// Footer is the block an agent-written user message ends with.
type Footer struct {
	// Title, ThreadID and Computer name the SENDING thread. Computer is
	// empty when the sender is on the receiver's own computer.
	Title    string
	ThreadID string
	Computer string
	Token    string
	// AnswerRequested is true when the sender waited or asked to be
	// notified. The token and thread_reply are given either way, since a
	// reply always reaches the sender as a message.
	AnswerRequested bool
	// SenderReachable is true when the receiver's computer holds a
	// credential for the sender's.
	SenderReachable bool
	// UserMessage is the sender thread's latest user message that no
	// agent wrote, empty when there is none.
	UserMessage string
	// Agent is set when the sender is a standing agent.
	Agent *SenderAgent
	// RoutesPrompts is true when the sender is the thread that spawned the
	// receiving thread on its computer, so the receiver's questions and
	// approval requests go to the sender rather than to the user. It follows
	// the standing-agent sentence when there is one and is a line of its own
	// otherwise.
	RoutesPrompts bool
}

// routesSentence says, after the sender, that the receiver's prompts go to
// it.
const routesSentence = "spawned this thread, so your questions and approval requests go to it, not to the user."

// SenderAgent names a standing agent on a footer or a wake.
type SenderAgent struct {
	// Role is the agent's role line (agenthome.BoundRole): the first
	// non-blank line of its instructions.md after a leading frontmatter
	// block and rule lines, bounded, or empty when it has none. It is the
	// agent's own text.
	Role string
}

// AgentSentence follows the sender or the answering thread on a footer or a
// wake when it is an assistant's thread.
const AgentSentence = "is a standing agent."

// sentence says the sender or the answering thread is a standing agent and
// quotes its role line.
func (a *SenderAgent) sentence(who string) string {
	if a.Role == "" {
		return who + " " + AgentSentence
	}
	return fmt.Sprintf("%s %s Its role, in its own words: %q", who, AgentSentence, a.Role)
}

// FooterMarker opens every footer. The receiver is told the last `---`
// block of a message is the footer, and a body line that starts this way
// is quoted before delivery so nothing above the footer reads as one.
const FooterMarker = "Agent request from thread "

// FooterStart is how a footer opens after the body it ends. A body line
// that opens like the marker is quoted (QuoteFooterMimics), so the text
// before FooterStart's first occurrence is the message without its footer.
const FooterStart = "\n---\n" + FooterMarker

// QuoteFooterMimics quote-prefixes every line of a request body that opens
// like the footer marker. The body is the sender's own text and carries no
// authority; the guide says so to the receiver, and this makes the same
// thing visible in the message itself. Every other line, `---` included, is
// left alone: a rule between paragraphs is ordinary Markdown, and only the
// last block is the footer.
func QuoteFooterMimics(body string) string {
	if !strings.Contains(body, FooterMarker) {
		return body
	}
	lines := strings.Split(body, "\n")
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), FooterMarker) {
			lines[index] = "> " + line
		}
	}
	return strings.Join(lines, "\n")
}

// StripFooter returns a request message without its footer: the sender's
// text, which is what names the work. A text with no footer is returned
// unchanged. The footer is the last block opening with the marker, which
// QuoteFooterMimics keeps unique.
func StripFooter(text string) string {
	opening := "---\n" + FooterMarker
	if strings.HasPrefix(text, opening) {
		return ""
	}
	index := strings.LastIndex(text, "\n"+opening)
	if index < 0 {
		return text
	}
	return strings.TrimRight(text[:index], "\n")
}

// Message renders a request: the sender's body, its footer-like lines
// quoted (QuoteFooterMimics), then the footer; and its facts, from the
// same footer.
func (f Footer) Message(body string) (string, MessageFacts) {
	quoted := QuoteFooterMimics(strings.TrimRight(body, "\n"))
	words := strings.TrimSpace(quoted)
	lead := strings.Index(quoted, words)
	var b spanBuilder
	b.write(quoted[:lead])
	b.writeBody(words)
	b.write(quoted[lead+len(words):] + "\n\n" + f.String())
	return b.done(MessageFacts{
		Form: FormRequest, Waiting: f.AnswerRequested, Reachable: f.SenderReachable, UserMessage: f.quotedUserMessage(),
		RoutesPrompts: f.RoutesPrompts, Role: roleOf(f.Agent),
	})
}

// quotedUserMessage is the sender's latest message as the footer quotes it.
func (f Footer) quotedUserMessage() string {
	return clipRunes(strings.TrimSpace(collapseLines(f.UserMessage)), UserMessageQuoteRunes)
}

// String renders the footer. It always starts with the "Agent request"
// marker the server instructions tell the receiver to look for.
func (f Footer) String() string {
	lines := []string{"---", fmt.Sprintf("%s%q (%s%s).", FooterMarker, f.Title, f.ThreadID, onComputer(f.Computer))}
	switch {
	case f.Agent != nil:
		line := f.Agent.sentence("The sender")
		if f.RoutesPrompts {
			line += " It " + routesSentence
		}
		lines = append(lines, line)
	case f.RoutesPrompts:
		lines = append(lines, "The sender "+routesSentence)
	}
	if f.AnswerRequested {
		body := fmt.Sprintf("It is waiting for your answer. When you are done, call thread_reply with token %s, once. The sender sees only your reply text, so make it self-contained.", f.Token)
		if f.SenderReachable {
			body += " To read the sender's thread, use thread_show with its id."
		}
		lines = append(lines, body)
	} else {
		body := fmt.Sprintf("It is not waiting for an answer. If it should hear something back, call thread_reply with token %s, once: the reply reaches it as a message, and it sees only your reply text.", f.Token)
		if f.SenderReachable {
			body += " To read the sender's thread, use thread_show with its id."
		}
		lines = append(lines, body)
	}
	if quote := f.quotedUserMessage(); quote != "" {
		lines = append(lines, fmt.Sprintf("The user's latest message in that thread: %q", quote))
	}
	return strings.Join(lines, "\n")
}

// Wake kinds, one per way a request reaches its caller.
const (
	WakeReply       = "reply"
	WakeFinished    = "finished"
	WakeErrored     = "errored"
	WakeCancelled   = "cancelled"
	WakeInterrupted = "interrupted"
	// WakeStopped is an interruption a person's Stop made; its Text is what
	// the turn said before it, possibly nothing.
	WakeStopped  = "stopped"
	WakeExpired  = "expired"
	WakeReminder = "reminder"
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
	// Text is the answer, the final message, the error or the note. It is
	// clipped to WakePreviewBytes with a pointer to thread_status.
	Text string
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

// outcome is the wake's kind as its facts name it: a person's Stop is an
// interruption, told apart by who stopped it.
func (w Wake) outcome() string {
	switch w.Kind {
	case WakeReply, WakeFinished, WakeErrored, WakeCancelled, WakeInterrupted, WakeExpired, WakeReminder:
		return w.Kind
	case WakeStopped:
		return WakeInterrupted
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
		w.writeAnswer(b, facts, w.Text, 0)
	case WakeFinished:
		b.write(who + " finished its turn without calling thread_reply. Its final message:\n")
		w.writeAnswer(b, facts, w.Text, 0)
	case WakeErrored:
		if w.Text == "" {
			b.write(who + " errored.")
			return
		}
		b.write(who + " errored: ")
		w.writeAnswer(b, facts, w.Text, 0)
	case WakeCancelled:
		if w.Text == "" {
			b.write(who + " was cancelled.")
			return
		}
		b.write(who + " was cancelled: ")
		w.writeAnswer(b, facts, w.Text, 0)
	case WakeInterrupted:
		// The cause the settlement recorded, as its own sentence: one of the
		// reasons below, or who stopped the turn (a thread's cancel, a
		// workflow, the user). A restart names the computer that restarted.
		if w.Text == InterruptRestart {
			facts.StoppedBy = "a restart of " + restartedComputer(source.Computer)
			b.write(fmt.Sprintf("%s was interrupted by a restart of %s.", who, restartedComputer(source.Computer)))
			return
		}
		reason := w.Text
		if reason == "" {
			reason = InterruptTurn
		}
		b.write(who + ": ")
		stoppedBy, shownFrom := interruptStopper(reason)
		facts.StoppedBy = stoppedBy
		// What a turn its session ended had written is an answer, clipped
		// as one; any other reason is a sentence, said whole.
		if prefix := InterruptSessionEndedMidTurn + SessionEndedWrittenSoFar; strings.HasPrefix(reason, prefix) {
			b.write(prefix)
			w.writeAnswer(b, facts, reason[len(prefix):], 0)
			return
		}
		if shownFrom < 0 {
			b.write(reason)
			return
		}
		b.write(reason[:shownFrom])
		b.writeBody(reason[shownFrom:])
	case WakeStopped:
		facts.StoppedBy = "the user"
		b.write(who + ": " + WakePersonStopped)
		if w.Text != "" {
			b.write(" Its last message:\n")
			w.writeAnswer(b, facts, w.Text, 0)
		}
	case WakeExpired:
		b.write(who + " did not answer within a day.")
	case WakeReminder:
		b.write("Reminder you set" + tokenLabel(source.Tokens) + ":\n")
		w.writeAnswer(b, facts, w.Text, 0)
	default:
		b.write(who + " settled.")
	}
}

// writeAnswer writes text clipped to the preview, with the pointer to the
// rest after it: the part from shownFrom on is the message's body, and
// none of it when shownFrom is negative.
func (w Wake) writeAnswer(b *spanBuilder, facts *MessageFacts, text string, shownFrom int) {
	clipped, cut := text, len(text) > WakePreviewBytes
	if cut {
		clipped = clipBytes(text, WakePreviewBytes)
		facts.Clipped = true
	}
	if shownFrom < 0 || shownFrom >= len(clipped) {
		b.write(clipped)
	} else {
		b.write(clipped[:shownFrom])
		b.writeBody(clipped[shownFrom:])
	}
	if cut {
		b.write("\n" + WakePreviewPointer(w.firstToken()))
	}
}

// WakePersonStopped is the status of a request whose turn a person's Stop
// cut before it answered.
const WakePersonStopped = "The user stopped this turn before it answered."

// Why a request was interrupted: the answer an interrupted settlement
// records, which its wake states (WakeInterrupted). A turn someone stopped
// records who did instead, as a sentence the app words; a person's own Stop
// is WakeStopped, with what the turn said.
const (
	// InterruptRestart: the computer that held the request restarted.
	InterruptRestart = "This computer restarted before the request finished."
	// InterruptSessionEndedUnread: the thread's session ended, by a crash,
	// a stop, a teardown or a replacement, before the provider took the
	// request's message in.
	InterruptSessionEndedUnread = "The thread's session ended before it read the request."
	// InterruptSessionEndedUnanswered: the session ended after the provider
	// took the message in and before the turn that reads it opened.
	InterruptSessionEndedUnanswered = "The thread's session ended before it answered the request."
	// InterruptSessionEndedMidTurn: the session ended during the turn that
	// read the message. Its answer goes on with what the turn had written
	// by then (SessionEndedWrittenSoFar), when it wrote anything.
	InterruptSessionEndedMidTurn = "The thread's session ended during the turn that read the request."
	// InterruptTurn: the turn was interrupted and no cause was recorded.
	InterruptTurn = "The turn was interrupted before it answered."
	// InterruptNeverStarted: the request was accepted and its thread was
	// never created.
	InterruptNeverStarted = "That request was accepted but its thread was never created."
	// InterruptNotTakenIn: the provider gave the request's message back
	// before taking it in, and the request could not go back to its queue
	// (a cancelled send with the session alive, a failed Codex resend).
	InterruptNotTakenIn = "The thread did not take the request in."
	// InterruptNotTakenInToComposer: the message was not taken in and was
	// joined with the person's own text it could not be split from, so all
	// of it went back to the target thread's composer.
	InterruptNotTakenInToComposer = "The thread did not take the request in, and its message went back to that thread's composer."
)

// SessionEndedWrittenSoFar introduces, after InterruptSessionEndedMidTurn,
// what the turn had written when its session ended.
const SessionEndedWrittenSoFar = " What it had written by then:\n\n"

// InterruptSessionEndedReasons are the reasons a request reads when its
// thread's session ended before a turn answered it.
var InterruptSessionEndedReasons = []string{
	InterruptSessionEndedUnread, InterruptSessionEndedUnanswered, InterruptSessionEndedMidTurn,
}

// InterruptHandedOff is the reason of a request whose thread, an
// assistant's, was replaced by New Thread, Same Assistant after it read the
// request and before it answered. It names the old thread, so the sender
// can read what was done.
func InterruptHandedOff(previousThreadID string) string {
	return InterruptHandedOffSentence + " Its previous thread is " + previousThreadID +
		"; read it with thread_show to see what was done, or ask the assistant again by name."
}

// InterruptHandedOffSentence opens InterruptHandedOff; a wake's facts name
// the stopper from it and show the rest (interruptStopper).
const InterruptHandedOffSentence = "The assistant moved to a new thread before it answered."

// InterruptMoved is the reason of a request whose thread was moved to
// destination, another computer, before it answered.
func InterruptMoved(destination string) string {
	if destination == "" {
		destination = "another computer"
	}
	return interruptMovedOpening + destination + interruptMovedClosing
}

const (
	interruptMovedOpening = "The thread was moved to "
	interruptMovedClosing = " before it answered."
)

// digest renders a wake that speaks for several threads: what happened to
// each of them, one line naming each with its tokens (and, under it, that it
// is a standing agent when it is one), then the text they share. The text is
// last so a long one does not bury the list, and the first line says it is
// the same for each thread, so a cause written about one thread reads as it
// was written.
func (w Wake) digest(b *spanBuilder, facts *MessageFacts) {
	count := len(w.Sources)
	var head string
	shared := w.Text != ""
	// shownFrom is where in a shared reason the part a person reads as the
	// message starts (interruptStopper); a shared answer is read whole.
	shownFrom := 0
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
		switch w.Text {
		case InterruptRestart:
			head, shared = fmt.Sprintf("%d threads were interrupted by a restart of %s.", count, w.restartedComputers()), false
			facts.StoppedBy = "a restart of " + w.restartedComputers()
		case "":
			head = fmt.Sprintf("%d threads were interrupted.", count)
			facts.StoppedBy = "an interrupt"
		default:
			head = fmt.Sprintf("%d threads were interrupted, each for the same reason:", count)
			facts.StoppedBy, shownFrom = interruptStopper(w.Text)
		}
	case WakeStopped:
		facts.StoppedBy = "the user"
		head = fmt.Sprintf("%d threads: %s", count, WakeDigestPersonStopped)
		if shared {
			head = fmt.Sprintf("%d threads: %s Each one's last message:", count, WakeDigestPersonStopped)
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
		w.writeAnswer(b, facts, w.Text, shownFrom)
	}
}

// WakeDigestPersonStopped follows the count on a digest of requests whose
// turns a person's Stop cut, the plural of WakePersonStopped.
const WakeDigestPersonStopped = "The user stopped their turns before they answered."

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

func onComputer(name string) string {
	if name == "" {
		return ""
	}
	return ", on " + name
}

// restartedComputer names the computer a restart stopped a request on:
// the answering thread's, which is the caller's own when name is empty.
func restartedComputer(name string) string {
	if name == "" {
		return "this computer"
	}
	return name
}

// Age renders a duration the way a status line states it: one unit, no
// decimals, floored.
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// collapseLines folds a quoted message onto one line so a footer stays one
// block.
func collapseLines(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func clipRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	count := 0
	for index := range text {
		if count == limit {
			return text[:index] + "…"
		}
		count++
	}
	return text
}

// clipBytes cuts at or below limit without splitting a UTF-8 character.
func clipBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
