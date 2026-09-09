# Codex ghost summaries

This package contains pure, idempotent summary rewrites used when a Codex
session ends with persisted background tool rows still running.

Keep storage, emission, and lifecycle in `internal/codexthread`.
`GhostSummary` maps blank input to `Session ended` and appends
`SessionEndedSuffix` to nonblank input exactly once.
