# Git integration

This package owns repository status, branches, worktrees, commits, and remote
operations used by the app. Keep process execution and parsing here; project
discovery belongs in internal/gitroot, diff rendering in internal/gitdiff, and
filesystem invalidation in internal/gitwatch.

## Contracts

- Invoke Git with explicit argument vectors and repository context. Do not build
  shell commands or depend on the user's shell startup files.
- Treat paths and refs as data. Use -- before pathspecs, validate ref names with
  Git, and preserve spaces, newlines, non-ASCII text, and leading dashes.
- Parse machine output intended for machines. Prefer NUL-delimited formats and
  explicit encodings over localized, column-oriented display output.
- Keep reads cancellable and bound output where callers do not need the whole
  stream. Include stderr and the operation in errors without exposing secrets.
- Mutations serialize through the repository lock. Report partial failure when
  a required follow-up read or filesystem operation fails after Git exits.
- Do not infer repository identity from a directory prefix. Linked worktrees and
  submodules require Git's common-dir and top-level answers.
- Preserve the distinction among no repository, an empty result, and a failed
  Git command. Callers use those states differently.
- Redact credentials in remote URLs from logs and user-facing errors.

Worktree operations confirm the target through Git before removing or reusing
it. Never recursively delete a path merely because it appears under a project
directory. Keep main-worktree resolution and stale registration rules in
internal/gitroot.

Tests use real temporary repositories for Git semantics. Parser fixtures cover
malformed and cross-version output, but mocked command success cannot prove
worktree, index, ref, or quoting behavior.
