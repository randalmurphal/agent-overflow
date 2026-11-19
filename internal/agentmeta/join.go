package agentmeta

import "strings"

// JoinSeparator is the visible rule between the parts of a JOINED queued
// message. It goes in BOTH the stored row summary and the text written to the
// provider, so the bubble the user reads back, the text the model receives,
// and the message a revert cuts at are one thing.
//
// Claude Code's own boundary-drain merge newline-joins the parts
// (`joinPromptValues`, src/cli/print.ts). AO uses a markdown rule instead
// because its copy is also the row a person reads, edits and reverts to, and
// two prompts run together across a bare newline read as one paragraph.
//
// It lives here rather than beside either joiner because two of them exist for
// one wire fact: the flush dispatcher joins a drain it can see coming
// (`internal/app/app_flush_dispatch_join.go`), and triage folds one the CLI
// made across separate drains (`internal/triage/claude_merge_fold.go`). The
// two must produce the same row.
const JoinSeparator = "\n\n---\n\n"

// JoinedPart is one member of a joined message as it appears in the joined
// text: its send id, empty for a member without one, its length in bytes,
// separator excluded, and its attribution. A joined message's member texts
// are free text, an answer's or a job output's included, and can hold the
// separator, so only these lengths split one back into its members, and
// only the attribution tells an agent's member from the person's.
type JoinedPart struct {
	SendID       string        `json:"sendId,omitempty"`
	Bytes        int           `json:"bytes"`
	Origin       string        `json:"origin,omitempty"`
	OriginThread *OriginThread `json:"originThread,omitempty"`
	OriginWake   *OriginWake   `json:"originWake,omitempty"`
}

// IsAgent reports whether the part is a message an agent or the app wrote.
func (p JoinedPart) IsAgent() bool {
	return IsAgentMessage(Meta{Origin: p.Origin})
}

// SplitJoinedParts cuts text into the members parts describe. ok is false
// when parts do not describe text exactly.
func SplitJoinedParts(text string, parts []JoinedPart) (members []string, ok bool) {
	if len(parts) == 0 {
		return nil, false
	}
	members = make([]string, 0, len(parts))
	at := 0
	for i, part := range parts {
		if i > 0 {
			if !strings.HasPrefix(text[at:], JoinSeparator) {
				return nil, false
			}
			at += len(JoinSeparator)
		}
		if part.Bytes < 0 || part.Bytes > len(text)-at {
			return nil, false
		}
		members = append(members, text[at:at+part.Bytes])
		at += part.Bytes
	}
	return members, at == len(text)
}
