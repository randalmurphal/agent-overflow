# Git diff commands

This package runs bounded Git commands and returns raw unified-patch bytes,
commit metadata, revision checks, and file content at a commit. Parsing patches
into render structures belongs to callers.

Workspace diffs include staged, unstaged, and untracked non-ignored files by
building a synthetic tree in a temporary object directory. Never write those
objects or the temporary index into the repository. Fresh repositories compare
against an empty tree. Branch review compares the merge base to the synthetic
worktree tree.

`Options.IgnoreWhitespace` passes `-w` to Git. Keep canonical hunk line
numbers and do not post-process patch text. Disable external diff drivers,
textconv, and color for deterministic machine-readable output.

Patch commands have a fixed byte ceiling and return an error when exceeded;
they do not return a partial patch. Context cancellation must terminate and reap
Git. Tests use temporary real repositories for worktree states, unusual paths,
refs, commit ordering, whitespace, output limits, and cancellation.
