// Package rollout reads Codex's own on-disk session state: the
// `state_5.sqlite` thread index and the append-only rollout JSONL files it
// points at.
//
// It is the Codex half of the session importer. `List` answers "which Codex
// sessions exist"; `Parse` turns one rollout file into `[]importir.Event` —
// the same `provider.ProviderEvent` vocabulary a live session produces, so
// the neutral import writer builds the same store rows triage does.
//
// Nothing here spawns a process, writes to Codex's state, or resolves the
// Codex home itself: every entry point takes the home (or a full path) from
// its caller. Read-only, by construction.
package rollout
