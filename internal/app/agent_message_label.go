package app

import (
	"slices"
	"strconv"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// promptLabel is the text that names a turn by the message that started it,
// where a list shows turns by their prompts (the review pane's Edits
// selector). A person's message is its text, theirs alone when Claude joined
// an agent's into it; an agent's is agentMessageLabel.
func promptLabel(text string, meta usermessage.Meta) string {
	if agentmeta.IsAgentMessage(meta.Agent()) {
		return agentMessageLabel(text, meta)
	}
	if person, _, ok := usermessage.SplitPersonParts(text, meta); ok {
		return person
	}
	return text
}

// agentMessageLabel names an agent's message as the timeline's agent row
// heads it: the sender of its first agent message, then that message's
// header line, as agentmeta.Describe reads them. The frontend heads the
// row from the same reading (agentMessageFromItem, agentMessage.ts), held to
// this by testdata/agent_message_labels.json.
func agentMessageLabel(text string, meta usermessage.Meta) string {
	described := agentmeta.Describe(text, meta.Agent())
	first := slices.IndexFunc(described, agentmeta.Described.IsAgent)
	if first < 0 {
		return text
	}
	message := described[first]
	sender := agentMessageSenderName(message)
	if message.Facts.Headline == "" {
		return sender
	}
	return sender + ": " + message.Facts.Headline
}

// agentMessageSenderName is who a message is from as its row's header names
// it: the thread it is attributed to, the threads a digest speaks for, this
// thread for its own reminder, or what wrote a report (its descriptor's
// sender), "A wake" for a report of a kind this computer does not know.
func agentMessageSenderName(message agentmeta.Described) string {
	if message.Origin == agentmeta.OriginAgentWake {
		if message.Report == nil {
			return "A wake"
		}
		return message.Report.Sender
	}
	switch {
	case len(message.Facts.Threads) > 1:
		// A digest speaks for several threads; no one of them sends it.
		return strconv.Itoa(len(message.Facts.Threads)) + " threads"
	case message.Facts.Outcome == threadtools.WakeReminder:
		return "This thread"
	case message.Thread != nil && message.Thread.Title != "":
		return message.Thread.Title
	}
	return "another thread"
}
