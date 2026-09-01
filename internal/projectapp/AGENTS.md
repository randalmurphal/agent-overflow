# internal/projectapp/

Store-backed application service for project lifecycle and persistence policy
exposed through the Wails project bindings.

## Layout

- `service.go` — service/dependency construction plus explicit project
  list/create/rename/archive/unarchive and drag-sort persistence.
- `workspace.go` — implicit repository-root identity and registered-workspace
  membership policy for project-scoped git operations.
- `identity.go` — `BackfillIdentity`, the once-per-boot pass that derives the
  repository identity of rows that have none.
- `worktree_setup.go` — project recipe validation, persistence, and clearing.
- `deletion.go` — the git-free persistent workflow footprint used by deletion
  preview and the destructive deletion race guard.
- `threadorder.go` — deterministic parent-before-child lock ordering for the
  `internal/app` deletion adapter.
- `service_test.go` / `deletion_test.go` / `identity_test.go` /
  `threadorder_test.go` — lifecycle, membership, setup, deletion-footprint,
  validation, error-contract, repository-identity, and lock-order coverage.

## Responsibility boundary

- What BELONGS here:
  - Basic project application operations that validate input and then compose
    `internal/store` project methods.
  - Creation's filesystem directory validation and default row construction.
  - Repository-root identity through `internal/project.EnsureForWorkspace`.
  - Derived repository identity (`remote_url` / `root_commit`) through the
    narrow `Identity` port: stamped at creation, on a workspace-ensure that
    CREATED a row, and once per boot by `BackfillIdentity`.
  - The policy that a project-scoped destructive git cwd is either the project
    root or a worktree returned by the narrow `WorkspaceResolver` port.
  - Project worktree-setup validation and persistence.
  - Store-only deletion projections and deterministic deletion lock ordering.
- What does NOT belong here:
  - Live project deletion execution, deletion worktree inspection, workflow
    cancellation, provider-session shutdown, filesystem cleanup, scheduler
    refresh, or thread teardown. That is one explicitly destructive
    `internal/app` saga.
    Moving it would require an App-shaped host interface and obscure invariant
    35: workflow cancellation MUST finish before the application shell acquires
    thread locks.
  - Git mutation and async worktree-setup execution. `internal/app` owns the real git
    integration; `internal/worktreesetupapp` owns setup-run coordination.
  - Wails registration, application DTO mirrors, or event emission.

## Invariants

- Preserve the binding-visible validation and unavailable-store error text;
  callers use these methods as the existing project wire surface.
- **Every writing method answers with a `Write`** (the row as it now stands,
  plus whether the write actually moved it), because `internal/app` broadcasts
  the row on `project:updated` and a write that changed nothing must announce
  nothing. Both halves are always populated: a no-op still answers with the
  current row, since the calling client applies that answer and a blank row
  would erase the project from its sidebar. `Create` and `UpdateSortPositions`
  are the two shapes that do not need the flag — a creation always changed
  something, and a reorder returns exactly the rows it wrote.
- **A new writing method must be classified** in `internal/app`'s
  `projectAppWrites`. `TestEveryProjectServiceMethodIsClassified` fails until it
  is, and `TestEveryProjectMutationCallSiteBroadcasts` then fails until every
  App call site broadcasts. A project mutation nothing announces is invisible to
  every other attached client and looks correct on the screen that made it.
- `Create` accepts only an existing directory, stores its absolute path, and
  derives the default name from the final path component.
- **Identity is derived, never declared, and never an error.** The `Identity`
  dep is nil-safe and answers `("", "")` for a path that is not a repository —
  the same value a missing deriver gives — so no caller needs a second shape
  for "identity is unavailable here". `BackfillIdentity` touches only rows
  where BOTH halves are empty, includes archived rows (an archived project can
  be unarchived and would otherwise be the one entry that never merges), and
  runs once per boot: no polling, no retry, one derivation per unidentified
  row. A repository that gains an origin later is picked up by the next boot.
- Rename changes the display name only. Archive hides rows from `List`;
  unarchive returns the refreshed persisted row.
- Sort order is delegated intact to the store's single-transaction dense
  position assignment. Do not reinterpret or partially normalize the IDs here.
- `WorkflowFootprint.SameAs` compares sorted identity sets, not counts. Project
  deletion relies on it to refuse rows created during its unlocked cleanup.
- Source workspaces are never accepted by spelling alone. Non-root paths must
  resolve through `WorkspaceResolver` to a registered project worktree.
