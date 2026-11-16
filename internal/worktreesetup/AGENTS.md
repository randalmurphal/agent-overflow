# `internal/worktreesetup`

Validates and executes a project's worktree setup recipe. Persistence and provisioning policy belong to callers; this package owns copy and command execution. Workflow profile files do not own this setting.

## Execution

`RunObserved` is the single engine and `Run` uses it with a no-op observer. Copy steps run first, then argv commands in authored order. One context deadline covers the whole recipe. Each command runs through `procutil.RunGroup`: timeout and cancellation stop its process group with SIGTERM, then SIGKILL, and a command stopped that way has failed whatever its exit status. A process a command leaves running in its group is kept (`KeepLeftovers`) until the backend exits; one that still holds the command's output after the drain grace fails the command. Stop on the first failure and include the bounded output tail. `ResolveSteps` must project exactly the steps the runner executes.

Commands are argv, not shell text. Run them in the new worktree with the inherited toolchain environment plus authoritative `AO_PROJECT_ROOT` and `AO_WORKTREE_PATH` values appended last.

## Copy safety

Globs are project-root-relative. Refuse absolute paths, traversal, authored `.git`, symbolic links at any level, and no-match globs. Skip wildcard-discovered `.git`. Open through rooted filesystem handles and publish each destination with a unique temporary file, sync, and rename. Copy an executable file through `procutil.WriteExecutable`: a setup command can run it next, and a process the app starts while it is open for writing would make that fail with "text file busy". On Linux that waits for process starts in progress after the write instead of holding every start off during it.

Tests use temporary roots and must cover failure cleanup, observation parity, timeout of descendant processes, kept processes, and path escape attempts.
