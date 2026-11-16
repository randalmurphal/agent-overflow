package usermessage

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Described is one message of a user row as every reader takes it: its
// text, its attribution, and facts that describe that text
// (threadtools.MessageFacts.Check), with the row's header line set.
type Described struct {
	// Text is the message's own text: the row's, or its member's when the
	// row holds several.
	Text   string
	SendID string
	// Origin, Thread and Wake are the message's attribution as stored, its
	// facts aside. Origin is "" for a member of the person's.
	Origin string
	Thread *OriginThread
	Wake   *OriginWake
	// Facts are what the message says.
	Facts threadtools.MessageFacts
}

// Body is what the message says in its own words.
func (d Described) Body() string { return d.Facts.BodyOf(d.Text) }

// IsAgent reports whether the message is an agent's or the app's, not the
// person's.
func (d Described) IsAgent() bool {
	return d.Origin == OriginAgentThread || d.Origin == OriginAgentWake
}

// Describe is the one reading of an agent's user row for a person, which the
// timeline (through ProjectRowMeta), the Edits selector and thread_show all
// take: each message the row holds, in order, nil for a row that is the
// person's.
//
// A message is read from the facts it was written with. Facts that do not
// describe its text are not guessed around: the message is shown whole
// (threadtools.Whole), as is one whose row records members that do not
// describe its text, which a joined row of unknown boundaries is. A
// message of an agent thread with no facts is one main wrote, and the
// legacy reader (readLegacy) reads its text: main's requests, several of
// which a provider may have joined, and main's answers for one thread.
// Anything else the app wrote carries its facts.
func Describe(text string, meta Meta) []Described {
	described, _ := describe(text, meta)
	return described
}

// describe is Describe, and whether its messages are the members the row
// records (JoinedParts).
func describe(text string, meta Meta) (described []Described, recorded bool) {
	if !IsAgentMessage(meta) {
		return nil, false
	}
	if len(meta.JoinedParts) > 1 {
		if members, ok := SplitJoinedParts(text, meta.JoinedParts); ok {
			for i, part := range meta.JoinedParts {
				described = append(described, describeMessage(members[i], part.SendID, part.Origin, part.OriginThread, part.OriginWake)...)
			}
			return described, len(described) == len(meta.JoinedParts)
		}
	}
	return describeMessage(text, meta.SendID, meta.Origin, meta.OriginThread, meta.OriginWake), false
}

// describeMessage reads one stored message: one Described, or several for a
// row main wrote whose requests a provider joined.
func describeMessage(text, sendID, origin string, thread *OriginThread, wake *OriginWake) []Described {
	one := Described{Text: text, SendID: sendID, Origin: origin, Thread: thread, Wake: wake, Facts: threadtools.Whole(text)}
	switch origin {
	case OriginAgentThread:
		if thread == nil || thread.Facts == nil {
			if legacy := readLegacy(text, sendID, thread); legacy != nil {
				return legacy
			}
			break
		}
		if thread.Facts.Check(text) == nil {
			one.Facts = thread.Facts.Clone()
		}
	case OriginAgentWake:
		if wake != nil && wake.Facts != nil && wake.Facts.Form == threadtools.FormReport && wake.Facts.Check(text) == nil {
			one.Facts = wake.Facts.Clone()
		}
	}
	one.Facts.Headline = headline(one)
	return []Described{one}
}

// headline is what a message's row header says it is about: the writer's
// headline, else the first line of its body, with a report's quoted data
// decoded for reading, else, for a digest, the threads it speaks for.
func headline(d Described) string {
	if d.Facts.Headline != "" {
		return d.Facts.Headline
	}
	body := d.Facts.BodyOf(d.Text)
	if d.Facts.Form == threadtools.FormReport && d.Wake != nil {
		switch ReportKind(d.Wake.Kind) {
		case ReportCompletion, ReportOpening, ReportHandOff:
			// A job's output and the app's prose are shown as written.
		default:
			body = ReadableWakeText(body)
		}
	}
	if line := FirstLine(body); line != "" || len(d.Facts.Threads) < 2 {
		return line
	}
	names := make([]string, 0, len(d.Facts.Threads))
	for _, thread := range d.Facts.Threads {
		names = append(names, firstNonEmpty(thread.Title, thread.ThreadID))
	}
	return strings.Join(names, ", ")
}

// ProjectMeta is meta as a reader of the row whose text is text receives
// it: every agent message it holds attributed with the facts Describe reads
// of it, and a row main wrote whose requests a provider joined given the
// members its text holds (JoinedParts), each with its sender. The stored row
// is not changed. ok is false for a row that is the person's.
func ProjectMeta(text string, meta Meta) (projected Meta, ok bool) {
	described, recorded := describe(text, meta)
	if described == nil {
		return meta, false
	}
	projected = meta
	if len(described) == 1 && !recorded {
		projected.OriginThread, projected.OriginWake = withFacts(described[0])
		return projected, true
	}
	// The row's own attribution names its first member, whose facts do not
	// describe the row's whole text.
	whole := Described{Text: text, Origin: meta.Origin, Thread: meta.OriginThread, Wake: meta.OriginWake, Facts: threadtools.Whole(text)}
	whole.Facts.Headline = headline(whole)
	projected.OriginThread, projected.OriginWake = withFacts(whole)
	projected.JoinedParts = make([]JoinedPart, 0, len(described))
	for _, d := range described {
		part := JoinedPart{SendID: d.SendID, Bytes: len(d.Text), Origin: d.Origin}
		if d.IsAgent() {
			part.OriginThread, part.OriginWake = withFacts(d)
		}
		projected.JoinedParts = append(projected.JoinedParts, part)
	}
	return projected, true
}

// withFacts is a copy of d's attribution carrying d's facts.
func withFacts(d Described) (*OriginThread, *OriginWake) {
	facts := d.Facts.Clone()
	switch d.Origin {
	case OriginAgentThread:
		thread := d.Thread.clone()
		if thread == nil {
			thread = &OriginThread{}
		}
		thread.Facts = &facts
		return thread, nil
	case OriginAgentWake:
		wake := d.Wake.clone()
		if wake == nil {
			wake = &OriginWake{}
		}
		wake.Facts = &facts
		return nil, wake
	}
	return d.Thread.clone(), d.Wake.clone()
}

// ProjectRowMeta is a user row's encoded meta as a client receives it
// (ProjectMeta), every key the typed Meta does not own kept. A row that is
// not an agent's, or whose meta does not decode, is returned as stored.
func ProjectRowMeta(text, meta string) string {
	if !strings.Contains(meta, OriginAgentThread) && !strings.Contains(meta, OriginAgentWake) {
		return meta
	}
	decoded, err := FromItem(store.Item{Meta: meta})
	if err != nil {
		return meta
	}
	projected, ok := ProjectMeta(text, decoded)
	if !ok || reflect.DeepEqual(projected, decoded) {
		return meta
	}
	encoded, err := MergeJoinedMeta(meta, projected)
	if err != nil {
		return meta
	}
	return encoded
}

// FirstLine is the first line of text that says something: a quote
// marker, a bare rule and surrounding space dropped, white space collapsed.
func FirstLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, ">"), " "))
		if line != "" && line != "---" {
			return strings.Join(strings.Fields(line), " ")
		}
	}
	return ""
}

// ReadableWakeText is a report's text with the escapes of its quoted data
// decoded for reading, quotes kept: the Go twin of the frontend's
// readableWakeText (wakeText.ts). An escaped quote or backslash-letter keeps
// its escape, and so does a control, format or line-separator character.
func ReadableWakeText(text string) string {
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
