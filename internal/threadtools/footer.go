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
