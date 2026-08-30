package codexghost

import "strings"

// SessionEndedSuffix is appended to a ghost row's summary so the
// timeline clearly labels why the launch landed in `errored`.
// Idempotent — see GhostSummary for the HasSuffix-guarded rewrite that
// keeps a re-start after a second crash from accumulating duplicate
// suffixes.
const SessionEndedSuffix = " — session ended"

// GhostSummary rewrites a backgrounded tool_call's summary for the
// Codex runtime retirement. Empty summaries fall back to "Session ended" as
// the standalone label (the leading em-dash would look cosmetic and
// weird without context); non-empty summaries get SessionEndedSuffix
// appended. Idempotent: a repeat call leaves the string unchanged, so
// replay-through-start (the session starts, we flip, Codex later
// replays item/started which re-upserts the row back to running, then
// a second subprocess crash triggers another flip) accumulates no noise.
func GhostSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "Session ended"
	}
	if strings.HasSuffix(summary, SessionEndedSuffix) {
		return summary
	}
	return summary + SessionEndedSuffix
}
