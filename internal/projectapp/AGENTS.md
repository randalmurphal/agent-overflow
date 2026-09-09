# internal/projectapp/

Store-backed application policy for project lifecycle, repository identity,
workspace membership, setup recipes, and deletion snapshots.

`internal/app` retains Wails bindings, event projection, git mutation, async
worktree setup, and the live deletion transaction. Project deletion must cancel
workflows before acquiring thread locks, then coordinate session shutdown,
filesystem cleanup, scheduler refresh, and thread teardown.

## Contracts

- `Create` accepts an existing directory, stores its absolute path, derives
  the default name from its final component, and records repository identity
  through `internal/project.EnsureForWorkspace`.
- Repository identity is derived through the `Identity` port. An unavailable
  or non-repository path yields empty identity fields rather than a second
  result shape. `BackfillIdentity` visits archived and active rows whose two
  identity fields are both empty, once per boot.
- A non-root workspace must resolve through `WorkspaceResolver` to a
  registered worktree. A caller-supplied spelling alone never authorizes a
  project-scoped git operation.
- Mutations return the current row and whether state changed. A no-op still
  returns the row to the initiating client, while `internal/app` emits
  `project:updated` only when `changed` is true.
- Classify every new service mutation in `projectAppWrites`.
  `TestEveryProjectServiceMethodIsClassified` and
  `TestEveryProjectMutationCallSiteBroadcasts` enforce the binding facade.
- Preserve binding-visible validation and unavailable-store errors.
- Keep sort assignment in the store's single transaction. Do not partially
  normalize or reinterpret the submitted IDs.
- `WorkflowFootprint.SameAs` compares sorted identity sets. The deletion
  transaction uses it to detect rows created during unlocked cleanup.
- `ThreadLockOrder` is deterministic and parent-before-child. Do not derive a
  second lock order in `internal/app`.
