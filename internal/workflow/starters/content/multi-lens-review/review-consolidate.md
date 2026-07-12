# Consolidate the parallel reviews

Reconcile the reviewer evidence into one decision. Merge duplicates, resolve
conflicts by checking the workspace, and separate material issues from optional
improvements. Approve only when no material correctness, maintainability,
security, or reliability issue remains. If changes are needed, make the fix
guidance ordered, specific, and sufficient for a new agent to act without
re-reading every reviewer transcript.

Write the consolidated narrative to the system-provided path. Finish with the
generated envelope only: `status` is `done`, `question`, or `stuck`; on `done`,
set `outputs.verdict` to `approve` or `changes-needed`, provide
`outputs.consolidated-review`, and include `outputs.fix-guidance` when changes
are needed.
