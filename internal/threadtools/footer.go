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
	// reply always reaches the request record.
	AnswerRequested bool
	// SenderReachable is true when the receiver's computer holds a
	// credential for the sender's.
	SenderReachable bool
	// UserMessage is the sender thread's latest user message that no
	// agent wrote, empty when there is none.
	UserMessage string
	// Agent is set when the sender is a standing agent.
	Agent *SenderAgent
	// RoutesPrompts is true when the sender is the standing agent that
	// spawned the receiving thread, so the receiver's questions and approval
	// requests go to the sender rather than to the user.
	RoutesPrompts bool
}

// SenderAgent names a standing agent on a footer or a wake.
type SenderAgent struct {
	// Role is the agent's role line (agenthome.BoundRole): the first
	// non-blank line of its instructions.md after a leading frontmatter
	// block and rule lines, bounded, or empty when it has none. It is the
	// agent's own text.
	Role string
}

// sentence says the sender or the answering thread is a standing agent and
// quotes its role line.
func (a *SenderAgent) sentence(who string) string {
	if a.Role == "" {
		return who + " is a standing agent, a thread the user promoted."
	}
	return fmt.Sprintf("%s is a standing agent, a thread the user promoted. Its role, in its own words: %q", who, a.Role)
}

// footerMarker opens every footer. The receiver is told the last `---`
// block of a message is the footer, and a body line that starts this way
// is quoted before delivery so nothing above the footer reads as one.
const footerMarker = "Agent request from thread "

// QuoteFooterMimics quote-prefixes every line of a request body that opens
// like the footer marker. The body is the sender's own text and carries no
// authority; the guide says so to the receiver, and this makes the same
// thing visible in the message itself. Every other line, `---` included, is
// left alone: a rule between paragraphs is ordinary Markdown, and only the
// last block is the footer.
func QuoteFooterMimics(body string) string {
	if !strings.Contains(body, footerMarker) {
		return body
	}
	lines := strings.Split(body, "\n")
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), footerMarker) {
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
	opening := "---\n" + footerMarker
	if strings.HasPrefix(text, opening) {
		return ""
	}
	index := strings.LastIndex(text, "\n"+opening)
	if index < 0 {
		return text
	}
	return strings.TrimRight(text[:index], "\n")
}

// String renders the footer. It always starts with the "Agent request"
// marker the server instructions tell the receiver to look for.
func (f Footer) String() string {
	lines := []string{"---", fmt.Sprintf("%s%q (%s%s).", footerMarker, f.Title, f.ThreadID, onComputer(f.Computer))}
	if f.Agent != nil {
		line := f.Agent.sentence("The sender")
		if f.RoutesPrompts {
			line += " It spawned this thread, so your questions and approval requests go to it, not to the user."
		}
		lines = append(lines, line)
	}
	if f.AnswerRequested {
		body := fmt.Sprintf("It is waiting for your answer. When you are done, call thread_reply with token %s, once. The sender sees only your reply text, so make it self-contained.", f.Token)
		if f.SenderReachable {
			body += " To read the sender's thread, use thread_show with its id."
		}
		lines = append(lines, body)
	} else {
		body := fmt.Sprintf("It is not waiting. No answer notification was requested. To reply anyway, call thread_reply with token %s, once.", f.Token)
		if f.SenderReachable {
			body += " To answer it, use thread_send with its id."
		}
		lines = append(lines, body)
	}
	if quote := clipRunes(strings.TrimSpace(collapseLines(f.UserMessage)), UserMessageQuoteRunes); quote != "" {
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
	WakeExpired     = "expired"
	WakeReminder    = "reminder"
)

// Wake is the status line and body of a message delivered to the thread
// that made a request.
type Wake struct {
	Kind string
	// Title, ThreadID and Computer name the thread that answered.
	// Computer is empty when it is the caller's own.
	Title    string
	ThreadID string
	Computer string
	Token    string
	// Age is how long ago the answer was written. Stated only past
	// AgeFloor.
	Age time.Duration
	// Text is the answer, the final message, the error or the note. It is
	// clipped to WakePreviewBytes with a pointer to thread_status.
	Text string
	// Agent is set when the answering thread is a standing agent.
	Agent *SenderAgent
}

// String renders one wake, status line first. A wake from a standing agent
// says so on the line after the status line.
func (w Wake) String() string {
	status := w.status()
	if w.Agent == nil || w.Kind == WakeReminder {
		return status
	}
	head, body, found := strings.Cut(status, "\n")
	line := w.Agent.sentence("That thread")
	if !found {
		return head + "\n" + line
	}
	return head + "\n" + line + "\n" + body
}

func (w Wake) status() string {
	who := fmt.Sprintf("%q (%s%s%s)", w.Title, w.ThreadID, onComputer(w.Computer), w.answered())
	token := ""
	if w.Token != "" {
		token = fmt.Sprintf(" [token %s]", w.Token)
	}
	switch w.Kind {
	case WakeReply:
		return fmt.Sprintf("Reply from %s%s:\n%s", who, token, w.body())
	case WakeFinished:
		return fmt.Sprintf("%s%s finished its turn without calling thread_reply. Its final message:\n%s", who, token, w.body())
	case WakeErrored:
		return fmt.Sprintf("%s%s errored: %s", who, token, w.body())
	case WakeCancelled:
		return fmt.Sprintf("%s%s was cancelled.", who, token)
	case WakeInterrupted:
		return fmt.Sprintf("%s%s was interrupted by a restart of %s.", who, token, restartedComputer(w.Computer))
	case WakeExpired:
		return fmt.Sprintf("%s%s did not answer within a day.", who, token)
	case WakeReminder:
		return fmt.Sprintf("Reminder you set%s:\n%s", token, w.body())
	default:
		return fmt.Sprintf("%s%s settled.", who, token)
	}
}

func (w Wake) body() string { return WakeBody(w.Text, w.Token) }

// WakeBody clips a wake body to the preview budget and points at the whole
// answer. A body inside the budget is returned unchanged.
func WakeBody(text, token string) string {
	if len(text) <= WakePreviewBytes {
		return text
	}
	return clipBytes(text, WakePreviewBytes) + "\n" + WakePreviewPointer(token)
}

// WakePreviewPointer is the notice a clipped wake body ends with.
func WakePreviewPointer(token string) string {
	return fmt.Sprintf("[preview; thread_status %s for the whole answer]", token)
}

func (w Wake) answered() string {
	if w.Age < AgeFloor {
		return ""
	}
	return ", answered " + Age(w.Age) + " ago"
}

func onComputer(name string) string {
	if name == "" {
		return ""
	}
	return ", on " + name
}

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
