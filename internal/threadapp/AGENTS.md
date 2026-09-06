# internal/threadapp/

Store-backed application service for thread creation, listing, metadata,
model selection, interaction mode, and the keyed action-lock registry shared
by application-shell thread workflows.

## Responsibility boundary

What belongs here:

- Thread row creation and validation after `internal/app` supplies model and git policy
  through narrow ports.
- List/get/archive/read/pin/rename/branch metadata operations.
- Sidebar thread groups (`groups.go`, migration v76): group CRUD, the group
  pin trio, and membership moves. Pure persistence — a group owns no
  process, workspace, or provider session — so none of it takes a thread
  action lock: the registry serializes ACTIONS on a live thread, and moving
  a sidebar row between groups is not one.
- Model-profile selection and the guarded provider-switch write.
- Chat/plan interaction-mode validation and persistence.
- Store-only fork validation, interrupted settlement, Codex anchor lookup, and
  Claude provider-id remap planning.
- The one keyed action-lock registry used by send, fork, delete, git,
  session-start, import, and retention workflows.

What stays in `internal/app`:

- Wails method signatures, comments, and DTOs.
- Provider session lifecycle and live config/mode application.
- Claude JSONL slicing and live-leaf reads; Codex JSON-RPC `thread/fork`.
- Git/forge subprocesses, worktree setup execution, settings mutation, event
  projection, attachment cloning, and destructive deletion cleanup.
- Session config-apply locking, which belongs to session runtime rather than
  thread action serialization.

## Invariants

- Project path and workspace path remain distinct. A linked worktree changes
  only the thread workspace/worktree fields; the project row remains the main
  repository identity.
- A provider switch is refused once the store reports provider-owned history,
  and clears session/lazy-fork state atomically when it is allowed.
- Branch metadata updates are workspace-wide and match both the supplied and
  canonical path spellings.
- A row-mutating operation returns `(row, changed, error)`. `internal/app`
  broadcasts the row on `thread:updated` so a second attached client
  converges without a refresh, and `changed` is the gate that keeps a write
  which changed nothing off the wire — SQLite counts a row as affected when
  the assignment restates the value it held, so a rows-affected count cannot
  answer this. The row comes back from the write's own transaction; only the
  no-change path (`rowOrCurrent`) pays a follow-up read, and only where the
  caller still owes its client a row.
- Thread action locks self-clean through `internal/keyedlock`; callers never
  delete registry entries manually.
- `LockMutable` is a separate, self-cleaning registry for ordinary edits and
  queue admission. Transfer reservation takes action then mutation; a mutation
  must never wait for action. This preserves composer saves during sends and
  edit-resend sagas. `CheckMutable` checks AO and native ownership while either
  lock is held, including tombstones whose display rows were deleted. Call it
  before edits or execution. `CheckCleanup` additionally permits deleting a
  confirmed outgoing move's local cache. It never releases an unconfirmed
  handoff, calls native background cleanup, or removes the journal/native
  retirement. Project/worktree cleanup uses that same distinction: retired
  caches are never reattached or resumed. Store-only bulk metadata checks within its own
  writer transaction instead. Neither registry is a cached ownership model.
- Public metadata reads use `GetOwnedThread`, the same SQL ownership view as
  lists. Internal `Store.GetThread` can still read a retained transfer cache;
  exposing that row would restore a retired owner on reconnect.
- Creation provenance is observed once, at creation, and never restated.
  `CreatedByDevice` arrives on the options struct (root reads it off the
  connection; this package only records it), and the git coordinates come
  from the `Workspace` port's `ObserveOrigin`. Both are write-once at the
  store layer, so a creation path that skips them leaves a row that can never
  acquire them — which is what `TestEveryNewThreadRecordsWhereItCameFrom` in
  `internal/app` exists to catch.
