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

`Router.handleInit` writes `session_ref` via
`store.UpdateSessionRef` on every `EventInit`. The store-level update
also clears `pending_fork_session_ref` in the same statement
(`Store.UpdateSessionRef` in `internal/store/threads.go`) so a
pre-committed fork cannot get re-forked on the next restart.

## Start, Resume, Fork

`App.startSessionNow` picks the right cursor per provider:

- **Claude** — passes `--resume <session_ref>` via `buildArgs` in
  `internal/provider/claude/session.go`. If `PendingForkRef` is
  populated it replaces `Resume` and the `ForkSession` flag is set,
  producing `--fork-session --resume <source-ref>` so the CLI replays
  from the source into a fresh session id.
- **Codex** — passes `ResumeThreadID = t.SessionRef`. The session
  selects `thread/resume` over `thread/start` when the ID is non-empty
  (see the method-dispatch switch in `Session.start` in
  `internal/provider/codex/session.go`). Codex has a native
  `thread/fork` wire method that `app_thread_fork.go` uses at fork
  time; the child thread's first start still goes through plain
  resume on its freshly-assigned id.

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

`App.runSessionStart` (in `app_session_start.go`) coalesces concurrent
starts on the same thread id (see
`TestSwitchThreadCoalescesConcurrentAutoResume` in
`app_bindings_test.go`). Failures synthesize an `EventError` through
triage with `content = "auto-resume failed: ..."` (the goroutine in
`App.SwitchThread` calls `emitErrorToThread`).

## Disconnects and Manual Reconnect

When Claude's subprocess exits (idle-watchdog fires, crash, or manual
close) the session emits `EventSessionStatus{Content: "disconnected"}`
— see `TestReadLoopEmitsDisconnectedOnExit` and
`TestCloseWaitsForDisconnectedHandler` in
`internal/provider/claude/session_test.go`. The frontend's
`ProviderStatusBanner.svelte` renders a "Session disconnected" banner
with a Reconnect button that calls `App.ReconnectSession` (in
`app_session_bindings.go` — stop then `startSession`). The stored
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
