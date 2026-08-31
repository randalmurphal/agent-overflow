# internal/projectapp/

Store-backed application service for project lifecycle and persistence policy
exposed through the Wails project bindings.

## Layout

- `service.go` — service/dependency construction plus explicit project
  list/create/rename/archive/unarchive and drag-sort persistence.
- `workspace.go` — implicit repository-root identity and registered-workspace
  membership policy for project-scoped git operations.
- `worktree_setup.go` — project recipe validation, persistence, and clearing.
- `deletion.go` — the git-free persistent workflow footprint used by deletion
  preview and the destructive deletion race guard.
- `threadorder.go` — deterministic parent-before-child lock ordering for the
  `internal/app` deletion adapter.
- `service_test.go` / `deletion_test.go` / `threadorder_test.go` — lifecycle,
  membership, setup, deletion-footprint, validation, error-contract, and
  lock-order coverage.

## Responsibility boundary

- What BELONGS here:
  - Basic project application operations that validate input and then compose
    `internal/store` project methods.
  - Creation's filesystem directory validation and default row construction.
  - Repository-root identity through `internal/project.EnsureForWorkspace`.
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
- `Create` accepts only an existing directory, stores its absolute path, and
  derives the default name from the final path component.
- Rename changes the display name only. Archive hides rows from `List`;
  unarchive returns the refreshed persisted row.
- Sort order is delegated intact to the store's single-transaction dense
  position assignment. Do not reinterpret or partially normalize the IDs here.
- `WorkflowFootprint.SameAs` compares sorted identity sets, not counts. Project
  deletion relies on it to refuse rows created during its unlocked cleanup.
- Source workspaces are never accepted by spelling alone. Non-root paths must
  resolve through `WorkspaceResolver` to a registered project worktree.
