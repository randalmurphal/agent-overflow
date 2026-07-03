package discussion

import (
	"strings"

	"agent-overflow/internal/store"
)

// BuildTurnPrompt renders the channel messages a speaker hasn't seen
// yet into a labeled transcript, followed by a your-turn instruction.
// Pure function: the app layer resolves which messages the speaker
// hasn't seen (via the store) and dispatches the result to the
// provider session — this package only shapes the text.
//
// Each message is labeled by who sent it: human posts as "Human",
// system posts (e.g. the conclusion notice) as "Moderator", and agent
// posts by their FromRole, falling back to "Participant" when FromRole
// is blank. When messages is empty the transcript header is omitted
// entirely rather than rendering a dangling "New messages" with
// nothing under it.
func BuildTurnPrompt(selfRole string, messages []store.ChannelMessage) string {
	var b strings.Builder
	if len(messages) > 0 {
		b.WriteString("New messages in the discussion:\n\n")
		for i, msg := range messages {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(turnPromptSpeakerLabel(msg))
			b.WriteString(":\n")
			b.WriteString(msg.Content)
		}
		b.WriteString("\n\n")
	}
	b.WriteString("It's your turn to contribute the next reply in this discussion. Stay in your role as ")
	b.WriteString(FormatRole(selfRole))
	b.WriteString(" and respond with your contribution only — your reply will be relayed to the other participants.")
	return b.String()
}

// turnPromptSpeakerLabel maps a channel message's origin to the label
// BuildTurnPrompt renders above its content.
func turnPromptSpeakerLabel(msg store.ChannelMessage) string {
	switch msg.FromType {
	case "human":
		return "Human"
	case "system":
		return "Moderator"
	default:
		role := strings.TrimSpace(msg.FromRole)
		if role == "" {
			return "Participant"
		}
		return role
	}
}
