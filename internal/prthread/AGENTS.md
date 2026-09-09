# `internal/prthread`

Pure formatting helpers for creating a thread from pull-request metadata and a
diff. Forge access, project resolution, persistence, and dispatch stay with
callers.

Keep GitHub PR and GitLab MR labels aligned with the frontend. Bound titles on
rune boundaries and inlined diffs on bytes with an explicit omission marker.
`FenceForContent` must choose a Markdown fence longer than any backtick run in
the embedded diff.
