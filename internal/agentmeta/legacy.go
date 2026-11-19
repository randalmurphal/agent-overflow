package agentmeta

import (
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/threadtools"
)

// The legacy reader: the agent thread messages main wrote, which carry no
// facts (git show origin/main:internal/threadtools/footer.go and
// internal/app/app_thread_tools_wake_message.go). main attributed two kinds
// of message to a thread, with no kind and no facts:
//
//   - a request, its body then main's footer, which had no standing-agent
//     or routing line (Footer.String there); a provider's join of several
//     put them one after another, separated by JoinSeparator, and recorded
//     no members (main had no JoinedParts);
//   - an answer for one thread (threadWakeAttribution there gave a wake for
//     several to the app), under a `thread-wake` send id, in one of main's
//     status lines (Wake.String there), which had no standing-agent line
//     and no past thread.
//
// main wrote no agent-wake row: those kinds came after it. A text that is
// none of these shapes is not read.

// readLegacy reads a message main wrote: one Described, several for joined
// requests, nil when text is no shape main wrote. A row whose attribution
// names no thread is attributed to the thread its text names, as a later
// request of a joined row is (legacySender).
func readLegacy(text, sendID string, thread *OriginThread) []Described {
	if strings.HasPrefix(sendID, legacyWakeSendIDRoot) {
		facts, named, ok := readLegacyWake(text)
		if !ok {
			return nil
		}
		if thread == nil {
			thread = named
		}
		one := Described{Text: text, SendID: sendID, Origin: OriginAgentThread, Thread: thread, Facts: facts}
		one.Facts.Headline = headline(one)
		return []Described{one}
	}
	return readLegacyRequests(text, sendID, thread)
}

// legacyWakeSendIDRoot opens the send id of every wake main queued
// (store.ThreadWakeSendID).
const legacyWakeSendIDRoot = "thread-wake"

const (
	legacyWaiting    = "It is waiting for your answer. When you are done, call thread_reply with token "
	legacyNotWaiting = "It is not waiting for an answer. If it should hear something back, call thread_reply with token "
	legacyUserQuote  = "The user's latest message in that thread: "
)

// legacyFooterStart opens main's footer after the body it ends.
const legacyFooterStart = "---\n" + threadtools.FooterMarker

// readLegacyRequests reads main's requests: each body with its footer, the
// next after JoinSeparator, and a member with no footer after the last one
// (what else a provider joined). The first is the row's own and keeps its
// attribution; a later one is attributed to the thread its footer names.
func readLegacyRequests(text, sendID string, lead *OriginThread) []Described {
	var described []Described
	at := 0
	for at < len(text) {
		rule := strings.Index("\n"+text[at:], "\n"+legacyFooterStart)
		if rule < 0 {
			break
		}
		rule += at
		footer, end, ok := readLegacyFooter(text, rule)
		if !ok {
			return nil
		}
		member := text[at:end]
		facts := threadtools.MessageFacts{
			Form: threadtools.FormRequest, Bytes: len(member),
			Waiting: footer.AnswerRequested, Reachable: footer.SenderReachable, UserMessage: footer.UserMessage,
		}
		body := text[at:rule]
		if words := strings.TrimSpace(body); words != "" {
			start := strings.Index(body, words)
			facts.Body = []threadtools.Span{{Start: start, End: start + len(words)}}
		}
		from := lead
		if len(described) > 0 || lead == nil {
			from = legacySender(footer, lead)
		}
		one := Described{Text: member, Origin: OriginAgentThread, Thread: from, Facts: facts}
		if len(described) == 0 {
			one.SendID = sendID
		}
		one.Facts.Headline = headline(one)
		described = append(described, one)
		if end == len(text) {
			return described
		}
		if !strings.HasPrefix(text[end:], JoinSeparator) {
			// Something follows a footer with no separator: not main's.
			return nil
		}
		at = end + len(JoinSeparator)
	}
	if len(described) == 0 {
		return nil
	}
	// The rest is a member with no footer: the person's, or a message that
	// names no sender.
	return append(described, Described{Text: text[at:], Facts: threadtools.Whole(text[at:])})
}

// readLegacyFooter reads main's footer whose rule line starts at rule: what
// it states, and where its last line ends. ok is false when it is not
// main's footer word for word.
func readLegacyFooter(text string, rule int) (footer threadtools.Footer, end int, ok bool) {
	lines := strings.SplitN(text[rule+len("---\n"):], "\n", 4)
	naming, found := strings.CutPrefix(lines[0], threadtools.FooterMarker)
	if !found || !strings.HasSuffix(naming, ").") {
		return footer, 0, false
	}
	title, inner, ok := readLegacyNaming(strings.TrimSuffix(naming, "."))
	if !ok {
		return footer, 0, false
	}
	footer.ThreadID, footer.Computer, _ = splitLegacyInner(inner)
	footer.Title = title
	if len(lines) < 2 {
		return footer, 0, false
	}
	answer := lines[1]
	switch {
	case strings.HasPrefix(answer, legacyWaiting):
		footer.AnswerRequested = true
		footer.Token, _, _ = strings.Cut(answer[len(legacyWaiting):], ", once")
	case strings.HasPrefix(answer, legacyNotWaiting):
		footer.Token, _, _ = strings.Cut(answer[len(legacyNotWaiting):], ", once")
	default:
		return footer, 0, false
	}
	footer.SenderReachable = strings.HasSuffix(answer, " To read the sender's thread, use thread_show with its id.")
	read := 2
	if len(lines) > 2 {
		if quoted, isQuote := strings.CutPrefix(lines[2], legacyUserQuote); isQuote {
			if message, err := strconv.Unquote(quoted); err == nil {
				footer.UserMessage = message
				read = 3
			}
		}
	}
	// main's footer, rendered again from what was read, is the text read.
	if strings.Join(lines[:read], "\n") != strings.TrimPrefix(mainFooter(footer), "---\n") {
		return footer, 0, false
	}
	end = rule + len("---\n")
	for _, line := range lines[:read] {
		end += len(line) + 1
	}
	return footer, end - 1, true
}

// mainFooter renders a footer as main's Footer.String did: this build's
// with neither a standing-agent nor a routing line, and the quoted user
// message as written.
func mainFooter(footer threadtools.Footer) string {
	quote := footer.UserMessage
	footer.UserMessage, footer.Agent, footer.RoutesPrompts = "", nil, false
	rendered := footer.String()
	if quote != "" {
		rendered += "\n" + legacyUserQuote + strconv.Quote(quote)
	}
	return rendered
}

// legacySender is the attribution of a later request of main's joined row:
// the thread its footer names, opened through the row's own sender's
// computer when the footer names that one. A thread on another computer the
// row does not identify is named and not opened.
func legacySender(footer threadtools.Footer, lead *OriginThread) *OriginThread {
	from := &OriginThread{ThreadID: footer.ThreadID, Title: footer.Title, Token: footer.Token, ComputerName: footer.Computer}
	if footer.Computer == "" {
		return from
	}
	if lead != nil && lead.ComputerName == footer.Computer && lead.ComputerID != "" {
		from.ComputerID = lead.ComputerID
		return from
	}
	from.ThreadID = ""
	return from
}

// readLegacyNaming reads `"title" (inner)` opening s: the title, what the
// parentheses hold, and ok.
func readLegacyNaming(s string) (title, inner string, ok bool) {
	quoted, err := strconv.QuotedPrefix(s)
	if err != nil {
		return "", "", false
	}
	title, err = strconv.Unquote(quoted)
	if err != nil {
		return "", "", false
	}
	rest, found := strings.CutPrefix(s[len(quoted):], " (")
	if !found {
		return "", "", false
	}
	inner, _, found = strings.Cut(rest, ")")
	return title, inner, found
}

// readLegacyNamingRest is readLegacyNaming with what follows the closing
// parenthesis.
func readLegacyNamingRest(s string) (title, inner, rest string, ok bool) {
	title, inner, ok = readLegacyNaming(s)
	if !ok {
		return "", "", "", false
	}
	quoted, _ := strconv.QuotedPrefix(s)
	return title, inner, s[len(quoted)+len(" (")+len(inner)+len(")"):], true
}

// splitLegacyInner reads a naming's parentheses: `id[, on computer][,
// answered age ago]`.
func splitLegacyInner(inner string) (threadID, computer, age string) {
	if before, after, found := strings.Cut(inner, ", answered "); found && strings.HasSuffix(after, " ago") {
		inner, age = before, strings.TrimSuffix(after, " ago")
	}
	threadID, computer, _ = strings.Cut(inner, ", on ")
	return threadID, computer, age
}

// legacyTail is one of main's status lines after a thread's naming and
// tokens: the outcome it says, whether its answer follows inline on the
// line, on the next line, or not at all, and who stopped it when it says.
type legacyTail struct {
	opening, outcome, body, stoppedBy string
}

// legacyTails are every legacyTail main wrote.
var legacyTails = []legacyTail{
	{" finished its turn without calling thread_reply. Its final message:", threadtools.WakeFinished, "next", ""},
	{" errored: ", threadtools.WakeErrored, "inline", ""},
	{" errored.", threadtools.WakeErrored, "none", ""},
	{" was cancelled.", threadtools.WakeCancelled, "none", ""},
	// The cause the settling code recorded, which main stated as written.
	{" was interrupted: ", threadtools.WakeInterrupted, "inline", ""},
	{" was interrupted.", threadtools.WakeInterrupted, "none", "an interrupt"},
	{" did not answer within a day.", threadtools.WakeExpired, "none", ""},
	{" settled.", threadtools.WakeSettled, "none", ""},
}

// readLegacyWake reads main's answer for one thread: its status line, then
// its answer, clipped to the preview with main's pointer after it. named is
// the thread the status line names (legacySender), nil for a reminder.
func readLegacyWake(text string) (facts threadtools.MessageFacts, named *OriginThread, ok bool) {
	facts = threadtools.MessageFacts{Form: threadtools.FormAnswer, Bytes: len(text)}
	head, _, hasNext := strings.Cut(text, "\n")
	next := len(head) + 1
	bodyFrom, bodyTo := -1, len(text)
	if rest, reminder := strings.CutPrefix(head, "Reminder you set"); reminder {
		tokens, after := cutLegacyTokens(rest)
		if after != ":" {
			return facts, nil, false
		}
		facts.Outcome, facts.Tokens, bodyFrom = threadtools.WakeReminder, tokens, next
	} else {
		naming, reply := strings.CutPrefix(head, "Reply from ")
		title, inner, rest, found := readLegacyNamingRest(naming)
		if !found {
			return facts, nil, false
		}
		var footer threadtools.Footer
		footer.ThreadID, footer.Computer, facts.Age = splitLegacyInner(inner)
		footer.Title = title
		facts.Tokens, rest = cutLegacyTokens(rest)
		if len(facts.Tokens) > 0 {
			footer.Token = facts.Tokens[0]
		}
		named = legacySender(footer, nil)
		if reply {
			if rest != ":" {
				return facts, nil, false
			}
			facts.Outcome, bodyFrom = threadtools.WakeReply, next
		} else {
			at := slices.IndexFunc(legacyTails, func(tail legacyTail) bool {
				return tail.body == "inline" && strings.HasPrefix(rest, tail.opening) || rest == tail.opening
			})
			if at < 0 {
				return facts, nil, false
			}
			tail := legacyTails[at]
			facts.Outcome, facts.StoppedBy = tail.outcome, tail.stoppedBy
			switch tail.body {
			case "inline":
				bodyFrom = len(head) - len(rest) + len(tail.opening)
			case "next":
				bodyFrom = next
			default:
				if hasNext {
					return facts, nil, false
				}
			}
		}
	}
	if bodyFrom < 0 || bodyFrom >= len(text) {
		return facts, named, true
	}
	if len(facts.Tokens) > 0 {
		if pointer := "\n" + threadtools.WakePreviewPointer(facts.Tokens[0]); strings.HasSuffix(text, pointer) && len(text)-len(pointer) >= bodyFrom {
			facts.Clipped, bodyTo = true, len(text)-len(pointer)
		}
	}
	if bodyFrom < bodyTo {
		facts.Body = []threadtools.Span{{Start: bodyFrom, End: bodyTo}}
	}
	return facts, named, true
}

// cutLegacyTokens reads ` [token X]` or ` [tokens X, Y]` opening rest.
func cutLegacyTokens(rest string) (tokens []string, after string) {
	for _, label := range []string{" [token ", " [tokens "} {
		if inner, found := strings.CutPrefix(rest, label); found {
			list, tail, closed := strings.Cut(inner, "]")
			if !closed {
				return nil, rest
			}
			return strings.Split(list, ", "), tail
		}
	}
	return nil, rest
}
