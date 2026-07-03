package discussion

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// conclusionMarkerPrefix is the case-insensitive marker a participant's
// final message line must start with (leading whitespace tolerated) to
// propose ending the discussion. See ParseConclusionProposal.
const conclusionMarkerPrefix = "conclude:"

// conclusionSummaryMaxRunes caps a proposal's summary length so an
// unbounded participant reply can't grow the eventual conclusion
// message (BuildConclusionMessage) without limit.
const conclusionSummaryMaxRunes = 500

// ParseConclusionProposal inspects only the LAST non-empty line of text
// for the CONCLUDE: marker (case-insensitive, leading whitespace
// tolerated). A marker anywhere earlier in the text is NOT a proposal —
// only a participant's final line counts, so trailing prose after a
// CONCLUDE: line (the model continuing to think out loud, or a later
// paragraph that isn't actually the last line) cancels the proposal.
// This is also what "latest stance" composes with at the app layer: a
// later turn that lacks the marker on its own last line rescinds
// whatever this turn proposed (see Deliberation.WithdrawConclusionProposal).
//
// The summary is everything after the marker on that line, trimmed and
// capped at conclusionSummaryMaxRunes (rune-safe — never splits a
// multi-byte rune). An empty summary is still a valid proposal
// (ok=true). CRLF line endings are tolerated. Empty or whitespace-only
// input returns ok=false.
func ParseConclusionProposal(text string) (summary string, ok bool) {
	line := lastNonEmptyLine(text)
	if line == "" {
		return "", false
	}

	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) < len(conclusionMarkerPrefix) || !strings.EqualFold(trimmed[:len(conclusionMarkerPrefix)], conclusionMarkerPrefix) {
		return "", false
	}

	summary = strings.TrimSpace(trimmed[len(conclusionMarkerPrefix):])
	summary = capRunes(summary, conclusionSummaryMaxRunes)
	return summary, true
}

// lastNonEmptyLine returns the last line in text carrying non-whitespace
// content, tolerating CRLF line endings (a trailing "\r" is stripped
// before the emptiness check). Returns "" when text has no non-empty
// line at all.
func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		return line
	}
	return ""
}

// capRunes truncates s to at most n runes without splitting a
// multi-byte rune. A no-op when s already has n runes or fewer.
func capRunes(s string, n int) string {
	if n < 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}

// ConclusionMessageInput is BuildConclusionMessage's parameter struct.
// Proposals and RoleByThreadID are keyed by participant thread ID;
// ParticipantsInOrder supplies the deterministic roster order the
// unanimous form renders in (map iteration order is not stable).
type ConclusionMessageInput struct {
	Unanimous           bool
	MaxTurns            int
	ParticipantsInOrder []string
	Proposals           map[string]string
	RoleByThreadID      map[string]string
}

// BuildConclusionMessage renders the system-authored conclusion message
// for either termination cause.
//
// The turn-limit form (Unanimous == false) is byte-identical to the
// original circuit-breaker text — callers persist and assert against
// it verbatim.
//
// The unanimous form leads with a summary line, then (if at least one
// participant left a non-empty summary) a blank line followed by one
// "<Role>: <summary>" line per participant in ParticipantsInOrder,
// skipping participants whose summary is empty. If every participant's
// summary is empty the whole trailing block is omitted rather than
// rendering empty "<Role>: " lines.
func BuildConclusionMessage(in ConclusionMessageInput) string {
	if !in.Unanimous {
		return fmt.Sprintf("Discussion concluded: reached the %d-turn limit.", in.MaxTurns)
	}

	var lines []string
	for _, threadID := range in.ParticipantsInOrder {
		summary := strings.TrimSpace(in.Proposals[threadID])
		if summary == "" {
			continue
		}
		role := strings.TrimSpace(in.RoleByThreadID[threadID])
		if role == "" {
			role = "Participant"
		}
		lines = append(lines, fmt.Sprintf("%s: %s", role, summary))
	}

	var b strings.Builder
	b.WriteString("Discussion concluded: all participants proposed to conclude.")
	if len(lines) > 0 {
		b.WriteString("\n\n")
		b.WriteString(strings.Join(lines, "\n"))
	}
	return b.String()
}
