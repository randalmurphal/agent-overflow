# Git root resolution

This package resolves a path to the repository's main working tree and lists
registered linked worktrees. It deliberately does not depend on internal/git.

## Resolution contract

MainRoot follows Git common-directory semantics. For a linked worktree,
--show-toplevel identifies the current worktree while --git-common-dir leads to
the main repository metadata. Resolve relative Git paths against the worktree
that produced them and canonicalize only after existence checks.

A directory containing a plausible .git file is not proof of a worktree.
Confirm candidates with Git and verify that the reported metadata belongs to
the repository being resolved. Stop walking when a .git entry is present but
malformed or unreadable; continuing upward can attach a nested path to the wrong
repository.

MainRoot rejects nonexistent starting paths. Keep that failure distinct from
not being in a repository so callers do not turn stale paths into projects.

RegisteredWorktrees reads Git registration even when a worktree directory has
disappeared. Preserve missing entries for cleanup and repair. Parse porcelain
output as records and do not derive registration from directory scans.

Tests use real temporary repositories, linked worktrees, malformed .git files,
relative gitdirs, missing worktree paths, symlinks, and nested repositories.
