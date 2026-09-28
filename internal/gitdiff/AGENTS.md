# Git diff commands

This package runs Git commands and returns raw unified-patch bytes, commit
metadata, revision checks, and file content at a commit. Parsing patches into
render structures belongs to callers.

Workspace diffs include staged, unstaged, and untracked non-ignored files by
building a synthetic tree in a temporary object directory. Never write those
objects or the temporary index into the repository. Fresh repositories compare
against an empty tree. Branch review compares the merge base to the synthetic
worktree tree.

`Options.IgnoreWhitespace` passes `-w` to Git. Keep canonical hunk line
numbers and do not post-process patch text. `Options.patchFlags` pins the
flags that make the output deterministic and parseable regardless of user
configuration.

Patches have no size limit and are never held whole. An `Open*` function
resolves both endpoints to object IDs and returns a `Diff`, read in chunks by
byte offset (`stream.go`). A worktree diff's snapshot lives in a directory
under the caller's root until `Close`. A failed Git exit is an error, never a
short patch. Closing a diff or cancelling a read terminates and reaps Git.
Tests use temporary real repositories for worktree states, unusual paths,
refs, commit ordering, whitespace, chunking, re-reads and cancellation.
