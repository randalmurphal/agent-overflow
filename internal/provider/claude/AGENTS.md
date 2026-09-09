# Claude provider

This package runs one Claude Code CLI process per active headless session and
normalizes its NDJSON stream into `provider.ProviderEvent`. Read
[claude-wire.md](../../../docs/references/claude-wire.md) before changing wire
parsing or control requests.

## Spawn and live configuration

Always pass `--forward-subagent-text`. Deliver `Config.SystemPrompt` through
a 0600 temporary file and remove it on every close and failed-spawn path.
Settings that the CLI can override from its own configuration must travel
through the inline `--settings` payload, with their names reserved from user
environment overrides.

Structured output is session-scoped through `--json-schema`; Claude ignores
per-turn output schemas. Emit one `--add-dir` per additional directory before
later flags. Sanitize every disallowed-tool value as one argv argument.

Prefer `PlanLiveUpdate` and `ApplyLiveUpdate` over restart. Validate every
axis before the first write and register pending confirmations in `preSend`.
Model, permission, thinking, effort, and fast-mode updates have different wire
confirmations; preserve partial state until each confirmation arrives. Returning
thinking control to the CLI default requires restart. Account switches do not:
the CLI reloads its canonical credential on the next request.

## Credentials

Provider sessions do not write credentials. `login.go` drives OAuth in an
isolated caller-provided home; the account layer owns installation. Only one
flow is active, a rejected callback consumes it, and superseding a flow must
cancel its unbounded completion wait.

Claude refresh tokens rotate at startup in a detached task. A short-lived probe
near the refresh window must use `ProbeConfig.ReadCredential` and wait for the
credential change before teardown. Do not replace this with a fixed delay.
The rate-limit probe may read a bounded regular credential file, but never
writes it.

## Session lifecycle

Serialize parser input and event emission. Preserve provider item IDs for tool
correlation and the provider session ID needed for native resume. Closing must
settle pending approvals and release process resources without emitting under
internal locks.

Resume leaf selection mirrors Claude's deserialization filters and repairs an
unusable file-order leaf to the deepest surviving row. Use
`sessionfork.TranscriptTypes` for row admission. Sidechain rows from both disk
and live envelopes never advance the root leaf.

Parser state is single-goroutine. Consuming accessors clear state and need one
documented lifecycle owner; multi-envelope state needs cleanup at result,
close, or bounded eviction. Repeated `system/init` is idempotent. Absence of
an optional wire field is unknown, not false or empty.

Every tool start receives a matching completion. A top-level model stop may
soft-close the round before a later result supplies authoritative accounting.
Session-cumulative usage is converted into per-turn deltas. Unknown or
malformed envelopes are logged with bounded deduplication and do not terminate
the read loop.

Cross-session inbox policy is explicit on every spawn; disabled means
`refuse`, and headless sessions never use `hold`. Peer renames use the
ordinary `/rename` send path. Unknown turn origin fails toward local.

Parser behavior, task lifecycle, session JSONL details, permission modes, and
fixtures belong in [claude-wire.md](../../../docs/references/claude-wire.md).
Use [sessionfork](sessionfork/AGENTS.md) for native fork and relocation rules
and [sessionimport](sessionimport/AGENTS.md) for read-only history conversion.
