# internal/threadapp/

Store-backed application policy for thread creation, metadata, groups, model
selection, interaction mode, ownership checks, fork preparation, deletion,
and the keyed action and mutation lock registries.

`internal/app` retains Wails methods, provider lifecycle and native fork
mechanics, git/forge processes, worktree setup, settings, attachments, event
projection, and destructive cleanup.

## Contracts

- Project and workspace paths remain distinct. Linking a worktree changes the
  thread workspace fields, not the project row.
- Refuse provider changes after provider-owned history exists. An allowed
  switch clears session and lazy-fork state atomically.
- Group operations are persistence-only and take no thread action lock.
- Row mutations return `(row, changed, error)`. Return the current row for a
  no-op and let `internal/app` emit `thread:updated` only for a change.
- Branch metadata updates apply to every thread sharing the workspace and
  match both supplied and canonical path spellings.
- `LockAction` serializes execution-oriented workflows.
  `LockMutable` separately protects edits, queue admission, transfer
  reservation, publication, and final deletion. Transfer takes action then
  mutation; a mutation path must never wait for action.
- Call `CheckMutable` while the relevant lock is held before edits or
  execution. It checks AO and native ownership, including deleted display rows
  retained for transfer. `CheckCleanup` additionally admits only confirmed
  outgoing transfer cleanup.
- `DeleteTree` performs slow provider and terminal cleanup before taking the
  mutation lock for final attachment cleanup and row deletion. Preserve action
  to mutation order. Uploads stage outside the mutation lock, then reacquire it
  for ownership-checked publication.
- `DeletePorts.CheckDelete` runs before cleanup and again under the final
  mutation lock. Recursive deletion applies it to every child.
- Public metadata reads use `GetOwnedThread`. Direct store reads may expose
  retained transfer caches and must not back public methods.
- Creation provenance is write-once. Every creation path supplies
  `CreatedByDevice` and observes git origin through the `Workspace` port.
  Keep `TestEveryNewThreadRecordsWhereItCameFrom` current.

The keyed registries self-clean through `internal/keyedlock`; callers never
remove entries manually.
