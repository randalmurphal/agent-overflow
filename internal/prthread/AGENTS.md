# `internal/prthread`

Pure formatting helpers for pull/merge request text. Forge access and
dispatch stay with callers.

Keep GitHub PR and GitLab MR labels aligned with the frontend.
`FenceForContent` must choose a Markdown fence longer than any backtick run in
the embedded content.
