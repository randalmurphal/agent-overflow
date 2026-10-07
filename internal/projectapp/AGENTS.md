# internal/projectapp/

Store-backed application policy for project lifecycle, repository identity,
workspace membership, setup recipes, and deletion snapshots.

`internal/app` retains Wails bindings, event projection, git mutation, async
worktree setup, and the live deletion transaction. Project deletion must cancel
workflows before acquiring thread locks, then coordinate session shutdown,
filesystem cleanup, scheduler refresh, and thread teardown.

## Contracts

- `Create` accepts an existing directory, stores its absolute path, derives
  the default name from its final component, and records repository
  identity. Every path that creates a project row, session import included,
  goes through `Create`, `CreateCheckout` or `EnsureForWorkspace` so the row is identified.
- Repository identity is derived through the `Identity` port. A git failure
  is recorded in `identity_error` beside the last good identity; a path that
  is missing or no longer a repository keeps the last identity.
  `RefreshIdentity` re-reads every row, archived included, once per boot and
  writes only rows that changed. `InspectFolder` answers a folder's identity
  without writing and returns a git failure as an error. `CreateCheckout`
  validates the expected repository inside registration. Only equal verified forge IDs match. Failed lookups preserve a known
  ID only when the private origin-change stamp is unchanged. URLs and
  commit ancestry never match projects. See [repository identity](../../docs/specs/remote-access.md#7d-project-identity).
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
