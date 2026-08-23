# Providers

Both supported providers run as subprocesses talking over stdio. Go brokers;
the provider process owns turn state.

## Process Model

- One provider process per **active** thread. Inactive threads hold no
  process; spawning happens on resume.
- Claude Code: `claude --output-format stream-json --input-format stream-json
  --verbose`. NDJSON both directions. Authentication is via the CLI's own
  OAuth flow — we don't see credentials.
- Codex: `codex app-server`. JSON-RPC 2.0 over stdio. Same one-process-per-thread
  shape that forge proved out.

## Session Identity

- `session_ref` on `threads` records the provider-side session file path
  (`~/.claude/...` or `~/.codex/...`). This is what resume feeds back in.
- Fork creates a new thread row; `parent_thread_id` records the lineage.
  Codex has a native `thread/fork` method. A Claude tail fork is LAZY:
  `pending_fork_session_ref` stamps the source session and the fork's
  first send spawns `claude --resume <source> --fork-session`, so the
  CLI itself copies the transcript at startup. An anchored (fork-at-turn
  / fork-from-message) Claude fork slices the source JSONL up front
  instead (`internal/provider/claude/sessionfork`) — anchors point at
  rows already on disk, so the slice is exact.
- Forking DURING an active turn is supported and is a snapshot "as if
  interrupted right now": the source is never interrupted and never
  mutated, and only the fork's clone is settled (running/streaming rows
  → errored with the " — interrupted" suffix, open turn rows closed with
  `stop_reason='interrupted'`). Codex issues `thread/fork` with NO
  `lastTurnId` — with no boundary on a mid-turn source it copies
  persisted history and appends the same turn-aborted marker a real
  interrupt writes, onto the fork's copy only, and a `lastTurnId` naming
  an in-progress turn is rejected outright. Claude PINS the lazy cut:
  the fork click captures the live session's canonical leaf into
  `pending_fork_resume_at`, and the fork's first send passes
  `--resume-session-at <cursor> --fork-session`, which makes the CLI cut
  its fork copy exactly at the pin even when the source has kept
  streaming since (spike-verified 2.1.237: rows after the cursor are
  dropped from the fork copy, and source uuids are preserved verbatim, so
  no provider-id remap is needed). The UNPINNED lazy path is forbidden
  whenever a live session is registered — it would snapshot the
  transcript at the fork's first send, minutes or turns later (2026-08-22
  incident: a fork keyed on the turn row alone deferred unpinned and its
  transcript was cut 44s after its timeline). "Live" is wider than "has
  an open turn row": the Claude CLI closes a turn on `end_turn` and then
  self-re-invokes when a background task completes — for hours, with no
  new turn row — so the transcript can grow whenever the session process
  exists. At spawn time the stored pin is repaired against the CLI's
  resume deserialization filters (`claude.ResolveForkResumeCursor`; the
  CLI filters BEFORE the cursor lookup, so a dangling-tool_use pin would
  hard-fail resume pre-init) to the deepest filter-surviving row at or
  before the pin's file position — never forward — and a pin the file has
  not received yet (stdout-to-disk append gap) gets a bounded wait, then
  falls back to the deepest on-disk cursor. The pin is captured BEFORE
  the SQLite clone, so the transcript can never be AHEAD of the timeline:
  a fork whose rows say " — interrupted" over an answer its transcript
  holds complete would be lying. The reverse skew — a partial block in
  the timeline that the transcript lacks — is the honest real-interrupt
  shape, since an interrupted row never promised its content landed.
  Forking seconds after a send, before either provider has written
  anything, legitimately yields a fork holding just the prompt that
  starts a fresh provider thread; a transcript that cannot be READ, by
  contrast, fails the fork rather than degrading to that answer.

## Approvals (Bidirectional)

```
provider stdout → Go parses → emits approval-request event (with request_id)
                                       ↓
                              frontend renders ApprovalPrompt
                                       ↓
                 user clicks → frontend → Go (RespondToApproval)
                                       ↓
                 Go marshals provider-specific → writes to provider stdin
                                       ↓
                              provider resumes turn
```

Both providers use the same frontend shape (`ApprovalRequest` with `kind`).
Provider-specific adaptation (`CanUseTool` vs Codex sandbox/approval policies)
stays inside the provider package.

Approval `kind` values that currently arrive from providers:

- `user-input` — multi-question form.
- `permission` — grant scope: `turn` or `session`.
- `file-change`, `file-read`, `command` — Codex sandbox approvals.
- `mcp-elicitation` — MCP server dynamic config request.

The frontend must render a branch for each kind it can receive. Missing
branches cause a silent dead-end.

## Rate Limits, Costs, Compaction

- Rate limit events surface through the provider package's normalizer and
  become a single event shape the frontend renders in the status bar.
- Turn cost/token accounting is persisted on turn completion.
  Context-window occupancy updates separately from provider context
  snapshots.
- Claude emits `compact_boundary` markers when it compacts context. These
  are preserved as timeline items so forked threads can re-derive state.

## What Goes Where

- Protocol parsing, framing, and session lifecycle → `internal/provider/{claude,codex}`.
- Event classification and routing → `internal/triage`.
- Persistence and retrieval → `internal/store`.
- Nothing provider-specific escapes `internal/provider/` into triage or store.

For Claude NDJSON specifics see `internal/provider/claude/AGENTS.md`.
For Codex JSON-RPC specifics see `internal/provider/codex/AGENTS.md`.
