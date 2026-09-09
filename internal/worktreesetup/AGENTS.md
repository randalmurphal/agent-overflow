# `internal/worktreesetup`

Validates and executes a project's worktree setup recipe. Persistence and provisioning policy belong to callers; this package owns copy and command execution. Workflow profile files do not own this setting.

## Execution

`RunObserved` is the single engine and `Run` uses it with a no-op observer. Copy steps run first, then argv commands in authored order. One context deadline covers the whole recipe, and command timeout kills the process group. Stop on the first failure and include the bounded output tail. `ResolveSteps` must project exactly the steps the runner executes.

Commands are argv, not shell text. Run them in the new worktree with the inherited toolchain environment plus authoritative `AO_PROJECT_ROOT` and `AO_WORKTREE_PATH` values appended last.

## Copy safety

Globs are project-root-relative. Refuse absolute paths, traversal, authored `.git`, symbolic links at any level, and no-match globs. Skip wildcard-discovered `.git`. Open through rooted filesystem handles and publish each destination with a unique temporary file, sync, and rename.

Tests use temporary roots and must cover failure cleanup, observation parity, timeout of descendant processes, and path escape attempts.
