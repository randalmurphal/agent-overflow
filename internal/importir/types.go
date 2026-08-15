package importir

import "agent-overflow/internal/provider"

// Event is one provider event recovered from a session file, plus the
// coordinates of the source line it came from.
//
// The embedded ProviderEvent is the same shape a live session produces, so
// the import writer builds the same store rows triage does. What it cannot
// carry is provenance: an imported event needs to name the row it was read
// from, both to stamp items.meta with it and so a refresh can find where the
// last import stopped.
//
// The two coordinates answer different questions and are NOT alternatives.
//
//   - SourceUUID is PROVENANCE and every reader must set it on every
//     item-producing event. Claude hands over the transcript row's own uuid;
//     Codex has no per-record id, so it mints `line:<byte offset of the
//     line's first byte>` — stable for the life of an append-only file,
//     unique within it, and prefixed so it can never be mistaken for a
//     number to do arithmetic on. It is what lands in
//     `items.meta.import_source_uuid`, and the writer refuses an event
//     without one.
//   - SourceOffset is an OPTIONAL resume position: the byte offset one past
//     the line's terminating newline, which a refresh cursor tails a Codex
//     rollout from. Claude leaves it zero — a transcript is a uuid DAG and a
//     byte offset says nothing about position in the conversation, so the
//     refresh there walks uuids instead.
type Event struct {
	provider.ProviderEvent
	SourceUUID   string `json:"sourceUuid,omitempty"`
	SourceOffset int64  `json:"sourceOffset,omitempty"`
}

// ModelProfile is the latest provider-recorded model configuration in the
// source region a reader converted. Empty fields mean the provider did not
// record that value; they are not permission to invent historical metadata.
//
// It is deliberately separate from Event. A profile is session/branch state,
// not a timeline row, and deriving it back out of turn or usage events couples
// import correctness to incidental event ordering and token accounting.
type ModelProfile struct {
	Model           string
	ReasoningEffort string
	ContextWindow   int
}

// Warning is a non-fatal finding from reading a session: something was
// skipped, could not be matched, or no longer exists. Warnings surface to
// the user next to the row they concern, so Message is prose, not a log
// line. Code groups warnings of the same kind for callers that dedupe.
//
// An import never fails on a warning — a session with one unreadable tool
// result is still worth importing.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
