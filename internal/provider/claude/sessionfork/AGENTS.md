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
  workspace's CANONICAL absolute path (symlinks resolved) encoded by
  `sanitizeProjectComponent` — every non-alphanumeric rune → `-`,
  mirroring Claude's `sanitizePath` (sessionStoragePortable.ts), NOT just
  path separators. (The earlier separators-only encoding silently missed
  for any path with `.`/`_`/`:` — i.e. nearly all of them — making the
  full-dir scan the de-facto path.) `MaxSanitizedSlugLen` (200) caps the
  slug; above it Claude appends a `Bun.hash` suffix Go can't reproduce, so
  `exactWorkspaceSlug` reports `ok=false` rather than guess. Falls back to
  scanning every project dir if the primary lookup misses.
- `relocate.go` — moves a session's transcript JSONL (+ its
  `<sessionID>/` subagent subdir) between project slugs so `claude
  --resume` run with cwd == destWorkspace resolves it. Claude keys resume
  on the cwd slug, so ANY workspace change — worktree create/attach,
  switch, or the removal reattach to the project root — would otherwise
  strand the transcript under the old slug ("No conversation found"). We
  move it, never clear the ref / start fresh. Split into the two halves of
  a move so the caller can sequence them around its own commit:
  - `RelocateSession(sessionID, fromWorkspace, destWorkspace) ->
    (srcFile, destFile, err)` is the COPY half. It writes the source (the
    authoritative latest — Claude only appends under the running cwd) OVER
    any stale copy at the destination, then leaves the source in place.
    Overwriting (not no-op-if-exists) is load-bearing: a thread returning
    to a workspace it visited before must resume the latest transcript,
    not the stale copy that visit left behind. `srcFile == destFile` means
    nothing moved (already at dest). Hard error with `destFile == ""` when
    the dest slug is uncomputable (over-length → unreproducible `Bun.hash`
    suffix) or the copy fails — an abortable caller refuses the change with
    the source intact. `ErrSessionFileNotFound` (both paths empty) when the
    transcript is genuinely gone — the caller surfaces it, never fabricates.
    `ErrSubagentCopyIncomplete` (destFile SET) is soft: resume works, only
    subagent history is partial.
  - `RemoveSessionTranscript(jsonlPath)` is the DELETE half: run on the
    pre-move source AFTER the workspace change commits, so the transcript
    follows the cwd as a single copy instead of accumulating stale
    duplicates under every slug visited. Idempotent on absent files; the
    `<id>/` subdir is derived from the JSONL basename, so path-traversal
    tokens (`.`/`..`, a bare `.jsonl`, or a name with no `.jsonl` suffix)
    are refused up front — otherwise a crafted basename could steer the
    `RemoveAll` off the session's own subdir (e.g. `...jsonl` → `..` →
    the whole projects dir). A post-commit failure leaves a harmless
    orphan (dest is authoritative + always overwritten), so callers log it.

  The COPY-before-commit / DELETE-after-commit ordering is what makes the
  change abort-safe: `app_worktree.go`'s `copyClaudeSessionForWorkspaceChange`
  copies all refs, and only `purgeRelocatedClaudeSessions` (post-commit)
  deletes the sources — a hard copy failure leaves every source in place so
  switch/create/attach refuse and the thread stays resumable where it is.
  Deletion can't abort (the worktree is already gone), so there the hard
  error surfaces and resume is left to fail loudly — bricked, never
  fabricated.

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
  - Locating session files in the standard Claude home layout, and
    moving them between project slugs (`RelocateSession` copy half +
    `RemoveSessionTranscript` delete half).
  - Atomic file writes (`O_EXCL` fork composer) and crash-safe streaming
    file/tree copies (temp + fsync + rename, no `io.ReadAll`).
- What does NOT belong here:
  - Decisions about *when* to fork or *when* to relocate, and the
    copy-then-commit-then-purge sequencing — all the caller's job.
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
