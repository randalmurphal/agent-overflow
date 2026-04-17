// Package replay persists per-thread NDJSON event logs so a user (or
// Agent Overflow maintainer) can reconstruct the exact sequence of
// triage/provider events that produced a thread's state. Replay is a debug
// aid, not a feature surface: it exists for the "what did the agent see
// right before it went wrong" question.
//
// The package exposes:
//
//   - Writer: per-thread, append-only writer with size-based rotation.
//   - Manager: a cache of writers keyed by thread id with idle cleanup.
//   - Record: the shape written to disk ({ts, threadId, kind, data}).
//
// Writes are fire-and-forget via a bounded channel; when the channel is
// full we drop the event and bump a metric rather than blocking the triage
// loop. Replay must never be on the critical path of rendering a turn.
//
// The manager is a no-op until the user enables the event log setting. It
// is safe to construct a disabled manager at app startup — no goroutines,
// no file handles.
package replay
