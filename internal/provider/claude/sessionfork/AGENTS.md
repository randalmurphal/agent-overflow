# internal/provider/claude/sessionfork/

Forks a Claude Code session JSONL transcript at an arbitrary past
message, producing a new `<newID>.jsonl` that `claude --resume <newID>`
can load with the truncated history. Mirrors Anthropic's official Python
SDK `fork_session(session_id, up_to_message_id)`:

```
~/repos/claude-agent-sdk-python/src/claude_agent_sdk/_internal/session_mutations.py
```

## Layout

- `forksession.go` — pure transform (`BuildForkLines`) plus atomic-write
  composer (`WriteForkFile`). Streaming `bufio.Scanner` with a 16 MB
  ceiling for multi-MB sessions. Exports `TranscriptTypes`, the row
  admission set shared with the claude package's branch validator
  (`sessionleaf_branch.go`) so the fork transform and the resume-at
  branch walk can never drift apart (invariant 28).
- `rechain.go` — `isDeferredAPIErrorRow`, the predicate behind the
  fork transform's re-chain rule (below).
- `findmessage.go` — `FindUUIDBeforeUserTurn` and `SliceUUIDForLastKeptTurn`:
  stream the JSONL and return the parentUuid of the Nth (0-indexed)
  **real** user prompt — i.e. the slice point that keeps everything
  through the previous turn's full assistant response. Filters
  tool-result echoes and sidechain entries. Early-terminates as soon
  as the requested index is found.
- `locate.go` — `LocateSessionFile`: resolves
  `~/.claude/projects/<slug>/<sessionID>.jsonl`. The slug is the
  workspace's CANONICAL absolute path (symlinks resolved) with separators
  replaced by `-`. Falls back to scanning every project dir if the
  primary lookup misses.

## Re-chain rule (invariant 28)

The CLI appends deferred `system/api_error` rows at the NEXT user send
with a **stale `parentUuid`** (upstream bug, 2.1.167–170 — see
claude-wire.md §"deferred system/api_error rows"). Copied positionally,
those rows leave the fork's writable tail off the active branch and
every resume of the fork hard-fails. `buildLines` therefore forces each
deferred api_error row's parent to its file predecessor (no-op when
already chained). Scope is strict:

- ONLY `type=="system" && subtype=="api_error"` rows are re-chained.
  Compact-boundary system rows are legitimate `parentUuid:null` chain
  ROOTS — a generic "system row off the chain" rule would corrupt them.
- User/assistant rows are NEVER re-chained — claude's own branch walk
  correctly ignores abandoned content branches.
- Known limitation: an abandoned-branch row immediately preceding an
  api_error becomes the forced parent — still strictly better than the
  hard failure (documented in rechain.go).

Fixture:
`docs/references/fixtures/claude/session_api_error_offbranch.jsonl`
(sanitized incident replica; rechain_test.go drives it through the
transform).

## Why JSONL manipulation, not CLI commands

`claude --fork-session` only forks at the latest message — there is no
CLI flag for "fork at past point". The Python SDK exposes
`fork_session(..., up_to_message_id=...)` precisely because the
transformation has to happen at the JSONL level. We port the same
recipe to Go so both providers (CLI-based Claude, RPC-based Codex)
support fork-at-point with consistent semantics.

## Responsibility boundary

- What BELONGS here:
  - JSONL parse / slice / UUID remap / parent-chain rewrite.
  - Locating session files in the standard Claude home layout.
  - Atomic file writes with `O_EXCL`.
- What does NOT belong here:
  - Decisions about *when* to fork (caller's job).
  - Updating thread rows / SessionRef plumbing — that's `app_thread_fork.go`
    or `app_checkpoint.go`.
  - Provider lifecycle (start/stop the Claude subprocess) — that's
    `internal/provider/claude/session.go`.
  - Codex anything — keep provider-specific code in its package.

## Anti-patterns

- Do NOT skip the canonical-path resolution in `LocateSessionFile`.
  On macOS `/tmp` and `/private/tmp` differ in the slug, and this is the
  most common cause of "session not found" bugs.
- Do NOT count `type:"user"` entries as user turns — many of them are
  tool-result echoes. Use `FindUUIDBeforeUserTurn` which filters by
  the `message.content` shape.
- Do NOT call `io.ReadAll` on session JSONLs. Real sessions can be
  multi-MB; everything in this package streams.

## References

- `~/repos/claude-agent-sdk-python/src/claude_agent_sdk/_internal/session_mutations.py:240-484`
  — Python source-of-truth (`fork_session` + `_build_fork_lines`).
- `~/repos/claude-code-source-code/src/commands/branch/branch.ts:90-145`
  — the CLI's own `/branch` command, same recipe in TypeScript.
