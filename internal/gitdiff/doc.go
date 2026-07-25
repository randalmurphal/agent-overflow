// Package gitdiff shells out to `git` for the review pane's diff
// sources: workspace-vs-HEAD, branch-base-to-worktree, per-commit
// patches, and the commit lists that back the per-commit selector.
// Worktree snapshots go through a temp GIT_INDEX_FILE so the user's
// index is never touched, and staging uses plumbing (`hash-object
// --no-filters` + `update-index`) so repo-defined clean filters are
// never executed by an automatic diff.
package gitdiff
