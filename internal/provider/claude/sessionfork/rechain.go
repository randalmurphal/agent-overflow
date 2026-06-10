// Deferred api_error re-chaining.
//
// Claude's CLI (observed on 2.1.167-2.1.170, incident 2026-06-10) writes
// `system/api_error` rows to the session JSONL LATE — at the NEXT user
// send after the API retry occurred — parent-chained to whatever the
// transcript leaf was WHEN THE RETRY FIRED, not to the row that
// precedes them in the file. When the retry happened mid-turn, the
// api_error rows' parent chain bypasses the rest of that turn
// (including its final assistant message), and the next user row chains
// onto the api_error rows. Claude reconstructs resumed context by
// walking parentUuid back from the file's LAST uuid-bearing transcript
// row, and validates `--resume-session-at` against that active branch
// only — so a fork that preserves this topology has its kept content
// tail OFF the active branch: resume-at any of those rows hard-fails at
// startup ("No message found with message.uuid of: ..."), and even a
// plain resume silently drops the bypassed tail from context.
//
// The fork transform therefore normalizes every deferred api_error row
// to chain at its FILE POSITION — parent = the previous writable row —
// which is exactly what a non-deferred write would have produced.
// Empirically verified against the incident file: the re-chained file
// resumes cleanly and `--resume-session-at` the final assistant row
// succeeds (claude 2.1.167/168/170). See
// docs/references/claude-wire.md §"Session JSONL: deferred
// system/api_error rows" and invariant 28.
//
// Scope is strictly `type=="system" && subtype=="api_error"`:
//
//   - Other system rows are NEVER moved. Compact-boundary system rows
//     are legitimate chain ROOTS (`parentUuid: null`, with the compact
//     summary user row chaining onto them) — a generic "system row off
//     the chain" rule would corrupt every compacted session.
//   - user/assistant rows are NEVER moved. Claude's own branch walk
//     correctly ignores abandoned content branches; resurrecting one
//     would corrupt resumed context.
//
// Known limitation: if claude ever interleaves an abandoned-branch row
// IMMEDIATELY before a deferred api_error row, the forced parent lands
// on that row. No such interleaving has been observed; the outcome
// would still be strictly better than today's hard resume failure.
package sessionfork

// isDeferredAPIErrorRow reports whether a transcript entry is a
// `system/api_error` row — the one row class whose source parentUuid is
// known-unreliable (see package comment above). Key shape pinned from
// the 2026-06-10 incident capture (claude 2.1.170): type "system",
// subtype "api_error", level "error", plus retryAttempt/retryInMs/
// maxRetries and an `error` object. Matching stays on type+subtype
// only — the narrowest stable discriminator.
func isDeferredAPIErrorRow(entry map[string]any) bool {
	if t, _ := entry["type"].(string); t != "system" {
		return false
	}
	subtype, _ := entry["subtype"].(string)
	return subtype == "api_error"
}
