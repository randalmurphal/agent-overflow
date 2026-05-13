# internal/diffreview/

Pure helpers behind the diff-review comment flow: rendering draft
comments as a plain-text prompt block the agent can read, picking the
best line anchor, and projecting a comment slice into its ID list.

The App-bound CRUD methods (`ListDiffReviewComments`,
`CreateDiffReviewComment`, `UpdateDiffReviewComment`,
`DeleteDiffReviewComment`), the `SendDiffReviewComments` saga that
turns drafts into a follow-up message, and the
`appendDiffReviewCommentsToContent` composer (which combines a draft
prompt with user content) stay in `app_diff_review_comments.go`.

## Surface

| Function | Purpose |
|---|---|
| `BuildPrompt(comments) string` | Renders draft comments into the agent-readable block. `<file_path>[:<line>]:\ncomment: <body>`, blank-separated, blank-body comments skipped. |
| `CommentLine(comment) int` | Prefers the new-side line, falls back to the old-side line, returns 0 for file-level comments so callers can omit the line entirely. |
| `IDsOf(comments) []string` | Projects a comment slice into its ID list, preserving order. Returns an empty (non-nil) slice for the empty input. |

## Design notes

- The pure helpers take `store.DiffReviewComment` directly — the
  store type is the shared shape, and re-mapping it locally would add
  conversion without buying clarity.
- The prompt-rendering rules (omit `side`, omit `selectedText`,
  prefer new-side line, two-newline separator) live here so the
  contract is captured in one place and tested directly. The agent
  sees the diff alongside the prompt, so the file:line anchor is
  enough to locate the comment.
