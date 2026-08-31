# Session Recovery

How a thread picks back up after a restart, a thread switch, or a
provider disconnect. The guiding rule is the same one in the core
principles: **the provider process is the source of truth during a
turn**, and the provider's on-disk session file (`~/.claude/...` or
`~/.codex/...`) is the authoritative history. SQLite stores the cursor
to hand back.

## Session Cursors

`store.Thread` carries two resume-related columns:

| Column | Meaning |
|---|---|
| `session_ref` | The provider-side session ID. For Claude it's the session file basename; for Codex it's the thread id. |
| `pending_fork_session_ref` | Set on a freshly-forked thread to point at the *source* session. Cleared the first time we start under it. |
| `pending_fork_resume_at` | The PIN for a lazy Claude fork taken off a LIVE source (migration v69): the source leaf uuid captured when Fork was clicked. Consumed with `pending_fork_session_ref`. Both session-ref writers clear the pair. Empty on an idle-source fork, whose tail IS the cut. |

`Router.handleInit` writes `session_ref` via
`store.UpdateSessionRef` on every `EventInit`. The store-level update
also clears `pending_fork_session_ref` and `pending_fork_resume_at` in
the same statement (`Store.UpdateSessionRef` in
`internal/store/threads.go`, mirrored by
`UpdateSessionRefAndRemapProviderIDs`) so a pre-committed fork cannot
get re-forked (or re-pinned) on the next restart.

## Start, Resume, Fork

`App.startSessionNow` picks the right cursor per provider:

- **Claude**: passes `--resume <session_ref>` via `buildArgs` in
  `internal/provider/claude/session_spawn.go`. If `PendingForkRef` is
  populated it replaces `Resume` and the `ForkSession` flag is set,
  producing `--fork-session --resume <source-ref>` so the CLI replays
  from the source into a fresh session id. If `PendingForkResumeAt` is
  also populated (a live-source tail fork), `startSessionNow` first
  resolves the pin through `resolveClaudeForkResumeAt`, then passes the
  result as `--resume-session-at <cursor>`, so the CLI cuts its fork
  copy at the pinned moment rather than the source's current tail.
  Resolving is a bounded wait for a pin still in the stdout-to-disk
  append gap, followed by `claude.ResolveForkResumeCursor`, which
  repairs a filter-dropped pin to the deepest surviving row at or
  before it (never forward).
- **Codex**: passes `ResumeThreadID = t.SessionRef`. The session
  selects `thread/resume` over `thread/start` when the ID is non-empty
  (see the method-dispatch switch in `Session.start` in
  `internal/provider/codex/session.go`). Codex has a native
  `thread/fork` wire method that `app_thread_fork.go` uses at fork
  time; the child thread's first start still goes through plain
  resume on its freshly-assigned id.

Forking a thread whose turn is still in flight produces a fork that
resumes exactly as an interrupted one does. Claude PINS the lazy cut:
the fork click captures the live session's `CanonicalLeafUUID`
(cold-scanning the file via `ScanSessionLeaf` when the process is
already gone) into `PendingForkResumeAt` alongside
`PendingForkRef = <source ref>`, and the fork's first send resolves the
pin and passes `--resume-session-at` (see above), so the CLI's own
fork cuts where the timeline was cloned rather than wherever the
source has grown to by then. Registration, not the turn row, is the
liveness test: the CLI self-re-invokes on background task completions
with the turn row long closed, and the transcript can grow whenever
the process exists (forkClaudeThread refuses the UNPINNED lazy branch
outright if a live session slipped past the capture). The pin is
captured before the clone runs, so it and the cloned timeline describe
one moment. Codex forks with no `lastTurnId` and gets back a
thread whose copy already carries the turn-aborted marker. When
nothing has been written yet, both refs stay empty and the fork's
first start is an ordinary `thread/start` / fresh Claude session,
but only for genuinely-early shapes (no session ref, no file yet, or
a transcript that parses and holds no settled leaf). A stat/open/size
failure reading the transcript fails the fork instead, so a fork can
never silently arrive with a full timeline and no history behind it.
The
fork's cloned rows are settled by
`store.SettleForkedThreadAsInterrupted`, which shares its item flip
and its `stop_reason='interrupted'` with `RecoverCrashedTurns`. The
fork is in the same position a crash leftover is, holding rows no
process will ever finish.

## Auto-Resume on SwitchThread

The frontend calls `SwitchThread(threadID)` whenever the user selects
a thread in the sidebar. The `App.SwitchThread` binding in
`app_session_bindings.go` looks up the thread, and when
`SessionRef != ""` or `PendingForkRef != ""` and no active session is
in the map, it launches `startSession` in a background goroutine:

```
SwitchThread(id) ──► store.GetThread ──► thread in sessions map?
                                          │ yes → return thread
                                          │ no  → cursor present?
                                          │        │ yes → go startSession(id)
                                          │        │ no  → return thread
```

`App.runSessionStart` (in `app_session_runtime.go`) coalesces concurrent
starts on the same thread id (see
`TestSwitchThreadCoalescesConcurrentAutoResume` in
`app_bindings_test.go`). Failures synthesize an `EventError` through
triage with `content = "auto-resume failed: ..."` (the goroutine in
`App.SwitchThread` calls `emitErrorToThread`).

## Disconnects and Manual Reconnect

When Claude's subprocess exits (idle-watchdog fires, crash, or manual
close) the session emits `EventSessionStatus{Content: "disconnected"}`.
See `TestReadLoopEmitsDisconnectedOnExit` and
`TestCloseWaitsForDisconnectedHandler` in
`internal/provider/claude/session_test.go`. The frontend's
`ProviderStatusBanner.svelte` renders a "Session disconnected" banner
with a Reconnect button that calls `App.ReconnectSession` (in
`app_session_bindings.go`: stop then `startSession`). The stored
`session_ref` drives the resume; no extra state is needed from the
frontend.

## Rollback and Recovery

Conversation rollback (fork-from-message and the Stop/Esc
revert-on-interrupt, shared saga in `app_conversation_rollback.go`)
interacts with this machinery:

- Codex calls `thread/rollback` on the live or a temp resumed session
  (`rollbackCodexThreadToMessage`). `session_ref` stays valid. Rolling
  back to turn 0 clears `session_ref` and starts fresh instead.
- Claude has no wire-level rollback, so
  `rollbackClaudeThreadToMessage` writes a sliced copy of the session
  JSONL (`internal/provider/claude/sessionfork`) and points
  `session_ref` at it; the original session file is left on disk
  untouched. Turn 0 clears the session entirely.

See `docs/architecture/revert-modes.md` for the full sequence.
