package usermessage

import (
	"regexp"
	"strconv"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
)

// A message Agent Overflow writes into a conversation reaches the model
// between a sender line and EndLine, so the model can tell every such
// message from the person's own, which carry neither. The marks are on the
// wire only: the stored row is the message as written, and its origin is
// what the timeline shows in their place.

const (
	// SenderPrefix opens the sender line of every marked message.
	SenderPrefix = "[Agent Overflow message"
	// EndLine closes a marked message.
	EndLine = "[End of Agent Overflow message]"
	// senderSuffix ends the sender line.
	senderSuffix = ". The user did not write it.]"
	// maxSenderName bounds a title or label a sender line quotes.
	maxSenderName = 200
)

// The OriginWake kinds of the app's messages other than a loop's or a
// watch's, whose kinds are store.AgentWakeLoop and store.AgentWakeWatch.
const (
	// OriginWakeKindSend is an outside `agent-overflow thread send`.
	OriginWakeKindSend = "send"
	// OriginWakeKindQuestion and OriginWakeKindApproval are a worker's
	// prompt put to the assistant that spawned it.
	OriginWakeKindQuestion = "question"
	OriginWakeKindApproval = "approval"
	// OriginWakeKindOpening and OriginWakeKindHandOff are the messages that
	// open an assistant's first thread and each later one.
	OriginWakeKindOpening = "assistant"
	OriginWakeKindHandOff = "handoff"
)

// WireText is text, a row's stored text with meta, as the model receives
// it on the computer whose backend id is here: each member the app or an
// agent wrote marked with its sender, the person's members as they are. A
// joined row is marked member by member while its parts describe it;
// otherwise the row is marked whole when it is an agent's message.
func WireText(text string, meta Meta, here string) string {
	if len(meta.JoinedParts) > 0 {
		if members, ok := SplitJoinedParts(text, meta.JoinedParts); ok {
			for i, part := range meta.JoinedParts {
				if sender, agent := senderOf(part.Origin, part.OriginThread, part.OriginWake, here); agent {
					members[i] = mark(members[i], sender)
				}
			}
			return strings.Join(members, JoinSeparator)
		}
	}
	if !IsAgentMessage(meta) {
		return text
	}
	sender, _ := senderOf(meta.Origin, meta.OriginThread, meta.OriginWake, here)
	return mark(text, sender)
}

// mark brackets text with the sender line and EndLine. An empty sender is
// the app itself. Text that would read as either mark is written with a
// parenthesis for its opening bracket, and for the end mark's closing one,
// so only the app's own marks open and close a message.
func mark(text, sender string) string {
	line := SenderPrefix
	if sender != "" {
		line += " from " + sender
	}
	return line + senderSuffix + "\n" + escapeMarks(text) + "\n" + EndLine
}

// markLike matches what a model could read as either mark: in any case,
// with any spacing or invisible formatting between its words, and after an
// ASCII or a fullwidth bracket. The second group is the end mark's closing
// bracket.
var markLike = func() *regexp.Regexp {
	gap := `[\s\p{Z}\p{Cf}]*`
	return regexp.MustCompile(`(?i)[\[［]` + gap + `((?:end` + gap + `of` + gap + `)?agent` + gap + `overflow` + gap + `message)(` + gap + `[\]］])?`)
}()

func escapeMarks(text string) string {
	if !strings.ContainsAny(text, "[［") {
		return text
	}
	return markLike.ReplaceAllStringFunc(text, func(found string) string {
		groups := markLike.FindStringSubmatch(found)
		if groups[2] != "" {
			return "(" + groups[1] + ")"
		}
		return "(" + groups[1]
	})
}

// senderOf names who a message with this attribution is from, as its
// sender line says it on the computer here, "" for the app itself; agent
// is false for the person's own message. A thread on another computer is
// named with that computer, by the name this computer gave the pairing
// when the request was accepted; one on this computer (its id here, or no
// id) is not, since naming it would read as a second machine.
func senderOf(origin string, thread *OriginThread, wake *OriginWake, here string) (sender string, agent bool) {
	switch origin {
	case OriginAgentThread:
		var from OriginThread
		if thread != nil {
			from = *thread
		}
		computer := ""
		if from.ComputerID != "" && from.ComputerID != here {
			computer = "another computer"
			if from.ComputerName != "" {
				computer = quoteName(from.ComputerName)
			}
		}
		switch {
		case from.Kind == OriginKindWake && from.ThreadID == "":
			return "a reminder you set", true
		case from.Kind == OriginKindWake:
			return "the request you made to " + threadName(from.Title, from.ThreadID, computer), true
		case from.ThreadID == "":
			return "the agent in another thread", true
		}
		return "the agent in " + threadName(from.Title, from.ThreadID, computer), true
	case OriginAgentWake:
		var from OriginWake
		if wake != nil {
			from = *wake
		}
		switch from.Kind {
		case store.AgentWakeLoop:
			return "your loop " + quoteName(from.Label), true
		case store.AgentWakeWatch:
			return "your watch " + quoteName(from.Label), true
		case OriginWakeKindSend:
			return "a script, through agent-overflow thread send", true
		case OriginWakeKindQuestion, OriginWakeKindApproval:
			return threadName(from.Label, from.ThreadID, "") + ", which you spawned", true
		case OriginWakeKindCompletion:
			job := "a remote job"
			if from.Label != "" {
				job = "the remote job " + quoteName(from.Label)
			}
			if from.Computer != "" {
				job += " on " + quoteName(from.Computer)
			}
			return job, true
		case OriginWakeKindWorkflow:
			return "the workflow run " + quoteName(from.Label), true
		}
		return "", true
	}
	return "", false
}

// threadName names a thread by its title and id, and by the computer it
// is on when computer, already quoted, is not empty.
func threadName(title, id, computer string) string {
	name := "thread " + quoteName(title)
	if title == "" {
		name = "a thread"
	}
	if id != "" {
		name += " (" + id + ")"
	}
	if computer != "" {
		name += " on " + computer
	}
	return name
}

// quoteName is a title or label as a sender line quotes it: on one line,
// bounded.
func quoteName(name string) string {
	return strconv.Quote(stringsx.ClipRunes(name, maxSenderName))
}
