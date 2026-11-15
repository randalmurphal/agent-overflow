package app

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
	"agent-overflow/internal/workflow/wake"
)

// remoteJobWakeHeader opens a remote job's completion (remoteCompletionMessage).
const remoteJobWakeHeader = "Remote job finished on "

// promptLabel is the text that names a turn by the message that started it,
// where a list shows turns by their prompts (the review pane's Edits
// selector). A person's message is its text, theirs alone when Claude joined
// an agent's into it; an agent's is agentMessageLabel.
func promptLabel(text string, meta usermessage.Meta) string {
	if usermessage.IsAgentMessage(meta) {
		return agentMessageLabel(text, meta)
	}
	if person, _, ok := usermessage.SplitPersonParts(text, meta); ok {
		return person
	}
	return text
}

// agentMessageLabel names an agent's message as the timeline's agent row
// heads it: the sender, then the first line of what it says, without the
// request's footer or the wake's template. It is the Go twin of the
// frontend's agentMessageFromItem sender and firstLine (agentMessage.ts),
// held to it by testdata/agent_message_labels.json.
func agentMessageLabel(text string, meta usermessage.Meta) string {
	sender, head := agentMessageHead(text, meta)
	if head == "" {
		return sender
	}
	return sender + ": " + head
}

func agentMessageHead(text string, meta usermessage.Meta) (sender, head string) {
	// A joined row is headed by its first member, read with that member's
	// own attribution. Without recorded parts the row is read whole.
	if members, ok := usermessage.SplitJoinedParts(text, meta.JoinedParts); ok && len(members) > 1 {
		first := meta.JoinedParts[0]
		text = members[0]
		meta = usermessage.Meta{SendID: first.SendID, Origin: first.Origin, OriginThread: first.OriginThread, OriginWake: first.OriginWake}
	}
	if meta.Origin == usermessage.OriginAgentWake {
		return agentWakeHead(text, meta.OriginWake)
	}
	var origin usermessage.OriginThread
	if meta.OriginThread != nil && meta.OriginThread.ThreadID != "" {
		origin = *meta.OriginThread
	}
	if origin.Kind == "wake" || origin.Kind == "" && strings.HasPrefix(meta.SendID, "thread-wake") {
		if titles, body, ok := readThreadWakeDigest(text); ok {
			// A digest speaks for several threads; no one of them sends it.
			if head = firstLineOf(body); head == "" {
				head = strings.Join(titles, ", ")
			}
			return strconv.Itoa(len(titles)) + " threads", head
		}
		if title, body, reminder, ok := readThreadWake(text); ok {
			if reminder {
				return "This thread", firstLineOf(body)
			}
			return firstNonEmpty(origin.Title, title, "another thread"), firstLineOf(body)
		}
	}
	body, title := firstRequestBody(text)
	return firstNonEmpty(origin.Title, title, "another thread"), firstLineOf(body)
}

// firstRequestBody is the body of a request row's first message and the
// sender title its footer names: the text before the first footer, whose
// body lines QuoteFooterMimics keeps from opening like one.
func firstRequestBody(text string) (body, title string) {
	const footer = "---\n" + threadtools.FooterMarker
	at := strings.Index("\n"+text, "\n"+footer)
	if at < 0 {
		return text, ""
	}
	line, _, _ := strings.Cut(text[at+len(footer):], "\n")
	if quoted, err := strconv.QuotedPrefix(line); err == nil {
		title, _ = strconv.Unquote(quoted)
	}
	return text[:at], title
}

// readThreadWake reads a wake an answer, a reminder or a settlement wrote
// (threadtools.Wake): the answering thread's title and the body after the
// status line and the standing-agent line. ok is false when text is not one.
func readThreadWake(text string) (title, body string, reminder, ok bool) {
	head, after, _ := strings.Cut(text, "\n")
	if rest, found := strings.CutPrefix(head, "Reminder you set"); found {
		if _, rest = cutWakeToken(rest); rest != ":" {
			return "", "", false, false
		}
		return "", after, true, true
	}
	reply := strings.HasPrefix(head, "Reply from ")
	naming := strings.TrimPrefix(head, "Reply from ")
	quoted, err := strconv.QuotedPrefix(naming)
	if err != nil || !strings.HasPrefix(naming[len(quoted):], " (") {
		return "", "", false, false
	}
	title, _ = strconv.Unquote(quoted)
	close := strings.Index(naming[len(quoted):], ")")
	if close < 0 {
		return "", "", false, false
	}
	_, rest := cutWakeToken(naming[len(quoted)+close+1:])
	if line, next, _ := strings.Cut(after, "\n"); strings.HasPrefix(line, "That thread "+threadtools.AgentSentence) {
		after = next
	}
	if reply {
		if rest != ":" {
			return "", "", false, false
		}
		return title, after, false, true
	}
	body, ok = wakeStatusBody(rest, after)
	return title, body, false, ok
}

// wakeStatusBody is the body of a wake whose status line goes on with rest
// after the thread and its tokens: the answer, the error, the cause or what
// the turn had written.
func wakeStatusBody(rest, after string) (body string, ok bool) {
	switch {
	case strings.HasPrefix(rest, " errored: "), strings.HasPrefix(rest, " was cancelled: "):
		_, inline, _ := strings.Cut(rest, ": ")
		body = inline + "\n" + after
	case strings.HasPrefix(rest, " finished its turn without calling thread_reply. Its final message:"),
		rest == ": "+threadtools.WakePersonStopped+" Its last message:":
		body = after
	case rest == ": "+threadtools.InterruptNeverStarted:
		body = threadtools.InterruptNeverStarted
	case rest == ": "+threadtools.InterruptSessionEndedMidTurn+strings.TrimRight(threadtools.SessionEndedWrittenSoFar, "\n"):
		body = strings.TrimPrefix(after, "\n")
	case strings.HasPrefix(rest, ": ") && slices.Contains(threadtools.InterruptSessionEndedReasons, rest[len(": "):]),
		rest == ": "+threadtools.WakePersonStopped, rest == ": "+threadtools.InterruptTurn,
		rest == ": "+threadtools.InterruptNotTakenIn, rest == ": "+threadtools.InterruptNotTakenInToComposer,
		strings.HasPrefix(rest, ": The thread was moved to ") && strings.HasSuffix(rest, " before it answered."),
		strings.HasPrefix(rest, " was cancelled."), strings.HasPrefix(rest, " was interrupted by a restart of "),
		rest == " did not answer within a day.", rest == " settled.":
	case strings.HasPrefix(rest, ": ") && after == "":
		// Who stopped the turn, worded by the stop (app_turn_stop.go).
		body = rest[len(": "):]
	case strings.HasPrefix(rest, " errored."):
	default:
		return "", false
	}
	return body, true
}

// readThreadWakeDigest reads a wake that speaks for several threads
// (threadtools.Wake digest): the titles it lists and the text after the
// list, read as a single wake's would be. ok is false when text is not one.
func readThreadWakeDigest(text string) (titles []string, body string, ok bool) {
	lines := strings.Split(text, "\n")
	count, opening, found := strings.Cut(lines[0], " threads")
	want, err := strconv.Atoi(count)
	if !found || err != nil || want < 2 {
		return nil, "", false
	}
	at := 1
	for ; at < len(lines) && strings.HasPrefix(lines[at], "- "); at++ {
		naming := lines[at][len("- "):]
		quoted, err := strconv.QuotedPrefix(naming)
		if err != nil || !strings.HasPrefix(naming[len(quoted):], " (") {
			return nil, "", false
		}
		title, _ := strconv.Unquote(quoted)
		if title == "" {
			if close := strings.Index(naming[len(quoted)+2:], ")"); close >= 0 {
				title, _, _ = strings.Cut(naming[len(quoted)+2:len(quoted)+2+close], ",")
			}
		}
		titles = append(titles, title)
		if at+1 < len(lines) && strings.HasPrefix(lines[at+1], "  That thread "+threadtools.AgentSentence) {
			at++
		}
	}
	if len(titles) != want {
		return nil, "", false
	}
	body = strings.Join(lines[at:], "\n")
	if opening == " were interrupted, each for the same reason:" {
		sentence, after, _ := strings.Cut(body, "\n")
		if reason, known := wakeStatusBody(": "+sentence, after); known {
			body = reason
		}
	}
	return titles, body, true
}

// cutWakeToken takes a status line's ` [token X]` or ` [tokens X, Y]` off
// the front of rest.
func cutWakeToken(rest string) (tokens, after string) {
	label := " [token "
	if !strings.HasPrefix(rest, label) {
		if label = " [tokens "; !strings.HasPrefix(rest, label) {
			return "", rest
		}
	}
	end := strings.Index(rest, "]")
	if end < 0 {
		return "", rest
	}
	return rest[len(label):end], rest[end+1:]
}

// agentWakeHead is the sender and first line of a loop's, a watch's, a
// script's, a worker's, a remote job's or a workflow's wake.
func agentWakeHead(text string, origin *usermessage.OriginWake) (sender, head string) {
	kind, label, computer := "send", "", ""
	if origin != nil {
		kind, label, computer = origin.Kind, origin.Label, origin.Computer
	}
	switch kind {
	case "send":
		sender = "A wake"
		if origin != nil {
			sender = "A script"
		}
	case "completion":
		sender = strings.Join(nonEmpty(label, computer), " on ")
		if sender == "" {
			sender = "A remote job"
		}
	case usermessage.OriginWakeKindOpening, usermessage.OriginWakeKindHandOff:
		// The app writes an assistant's opening and handoff blocks; they
		// are prose, not quoted data.
		return "Agent Overflow", firstLineOf(text)
	default:
		sender = firstNonEmpty(label, agentWakeKindTitles[kind])
	}
	switch kind {
	case "completion":
		for _, line := range strings.Split(text, "\n") {
			if result, found := strings.CutPrefix(line, "Result: "); found {
				return sender, strings.TrimSpace(result)
			}
		}
		return sender, firstLineOf(agentWakeBody(text))
	case "workflow":
		return sender, firstLineOf(agentWakeBody(text))
	}
	return sender, agentWakeDataLine(text)
}

// agentWakeKindTitles name a wake by its kind, as the frontend's agent row
// titles it, when it carries no label.
var agentWakeKindTitles = map[string]string{
	"loop":     "Loop wake",
	"watch":    "Watch wake",
	"question": "Worker's question",
	"approval": "Worker's approval request",
	"workflow": "Workflow wake",
}

// agentWakeDataLine is the first quoted data line of a wake, whose own first
// line is the template.
func agentWakeDataLine(text string) string {
	_, rest, _ := strings.Cut(text, "\n")
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimPrefix(line, "note: ")
		quoted, err := strconv.QuotedPrefix(line)
		if err != nil {
			continue
		}
		if value, err := strconv.Unquote(quoted); err == nil && strings.TrimSpace(value) != "" {
			return strings.Join(strings.Fields(value), " ")
		}
	}
	return firstLineOf(rest)
}

// agentWakeBody is a wake without its data notice line. A remote job's
// output follows its header as written; other data lines are quoted.
func agentWakeBody(text string) string {
	first, rest, _ := strings.Cut(text, "\n")
	if strings.HasPrefix(first, remoteJobWakeHeader) {
		return rest
	}
	if !strings.HasSuffix(first, " "+agentWakeDataNotice) && first != wake.DataNotice {
		rest = text
	}
	return readableWakeText(rest)
}

// readableWakeText is a wake's text with the escapes of its quoted data
// decoded for reading, quotes kept: the Go twin of the frontend's
// readableWakeText (wakeText.ts). An escaped quote or backslash-letter keeps
// its escape, and so does a control, format or line-separator character.
func readableWakeText(text string) string {
	return quotedWakeToken.ReplaceAllStringFunc(text, func(token string) string {
		return wakeEscape.ReplaceAllStringFunc(token, func(escape string) string {
			if escape == `\\` {
				return `\`
			}
			var digits string
			switch escape[1] {
			case 'x', 'u', 'U':
				digits = escape[2:]
			default:
				return escape
			}
			code, err := strconv.ParseUint(digits, 16, 32)
			if err != nil || code > unicode.MaxRune {
				return escape
			}
			r := rune(code)
			if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Zl, unicode.Zp) || r >= 0xD800 && r <= 0xDFFF {
				return escape
			}
			return string(r)
		})
	})
}

var (
	quotedWakeToken = regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"`)
	wakeEscape      = regexp.MustCompile(`\\(?:x[0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}|.)`)
)

// firstLineOf is the first line of text that says something: a quote
// marker, a bare rule and surrounding space dropped, whitespace collapsed.
func firstLineOf(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, ">"), " "))
		if line != "" && line != "---" {
			return strings.Join(strings.Fields(line), " ")
		}
	}
	return ""
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
