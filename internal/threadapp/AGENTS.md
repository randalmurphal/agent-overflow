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
- Thread action locks self-clean through `internal/keyedlock`; callers never
  delete registry entries manually.
