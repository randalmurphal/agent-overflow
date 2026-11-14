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

// String renders the footer. It always starts with the "Agent request"
// marker the server instructions tell the receiver to look for.
func (f Footer) String() string {
	lines := []string{"---", fmt.Sprintf("%s%q (%s%s).", footerMarker, f.Title, f.ThreadID, onComputer(f.Computer))}
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

// WakeSource is one thread a wake speaks for, with the requests of it the
// wake answers.
type WakeSource struct {
	// Title, ThreadID and Computer name the thread that answered. Computer
	// is empty when it is the caller's own. All three are empty for a
	// reminder, which no thread answers.
	Title    string
	ThreadID string
	Computer string
	// Tokens are the requests this wake answers, oldest first. One turn can
	// answer several: it consumed several of the caller's messages.
	Tokens []string
	// Age is how long ago the answer was written. Stated only past
	// AgeFloor.
	Age time.Duration
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
}

// String renders one wake, status line first.
func (w Wake) String() string {
	if len(w.Sources) > 1 {
		return w.digest()
	}
	var source WakeSource
	if len(w.Sources) == 1 {
		source = w.Sources[0]
	}
	who := source.who()
	token := tokenLabel(source.Tokens)
	switch w.Kind {
	case WakeReply:
		return fmt.Sprintf("Reply from %s%s:\n%s", who, token, w.body())
	case WakeFinished:
		return fmt.Sprintf("%s%s finished its turn without calling thread_reply. Its final message:\n%s", who, token, w.body())
	case WakeErrored:
		if w.Text == "" {
			return fmt.Sprintf("%s%s errored.", who, token)
		}
		return fmt.Sprintf("%s%s errored: %s", who, token, w.body())
	case WakeCancelled:
		return fmt.Sprintf("%s%s was cancelled.", who, token)
	case WakeInterrupted:
		// Text is the cause the settling code recorded: a person stopped the
		// turn, the computer running it restarted, the thread moved. The kind
		// alone does not say which.
		if w.Text == "" {
			return fmt.Sprintf("%s%s was interrupted.", who, token)
		}
		return fmt.Sprintf("%s%s was interrupted: %s", who, token, w.body())
	case WakeExpired:
		return fmt.Sprintf("%s%s did not answer within a day.", who, token)
	case WakeReminder:
		return fmt.Sprintf("Reminder you set%s:\n%s", token, w.body())
	default:
		return fmt.Sprintf("%s%s settled.", who, token)
	}
}

// digest renders a wake that speaks for several threads: what happened to
// each of them, one line naming each with its tokens, then the text they
// share. The text is last so a long one does not bury the list, and the
// first line says it is the same for each thread, so a cause written about
// one thread ("the computer running it restarted") reads as it was written.
func (w Wake) digest() string {
	count := len(w.Sources)
	var head string
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
		head, shared = fmt.Sprintf("%d threads were cancelled.", count), false
	case WakeInterrupted:
		head = fmt.Sprintf("%d threads were interrupted.", count)
		if shared {
			head = fmt.Sprintf("%d threads were interrupted, each for the same reason:", count)
		}
	case WakeExpired:
		head, shared = fmt.Sprintf("%d threads did not answer within a day.", count), false
	default:
		head, shared = fmt.Sprintf("%d threads settled.", count), false
	}
	lines := make([]string, 0, count+2)
	lines = append(lines, head)
	for _, source := range w.Sources {
		lines = append(lines, "- "+source.who()+tokenLabel(source.Tokens))
	}
	if shared {
		lines = append(lines, w.body())
	}
	return strings.Join(lines, "\n")
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

func (w Wake) body() string { return WakeBody(w.Text, w.firstToken()) }

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

func answered(age time.Duration) string {
	if age < AgeFloor {
		return ""
	}
	return ", answered " + Age(age) + " ago"
}

func onComputer(name string) string {
	if name == "" {
		return ""
	}
	return ", on " + name
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
