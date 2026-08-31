# internal/diffreview/

Pure helpers behind the diff-review comment flow: rendering draft
comments as a plain-text prompt block the agent can read, picking the
best line anchor, and projecting a comment slice into its ID list.

The App-bound CRUD methods (`ListDiffReviewComments`,
`CreateDiffReviewComment`, `UpdateDiffReviewComment`,
`DeleteDiffReviewComment`), the `SendDiffReviewComments` saga that
turns drafts into a follow-up message, and the
`appendDiffReviewCommentsToContent` composer (which combines a draft
prompt with user content) stay in `app_review_comments.go`.

## Surface

| Function | Purpose |
|---|---|
| `BuildPrompt(comments) string` | Renders draft comments into the agent-readable block. `<file_path>[:<line>]:\ncomment: <body>`, blank-separated, blank-body comments skipped. |
| `BuildPromptWithPRContext(comments, pr) string` | Same block with a `PR #<n> - <url>` header and, per comment, the `hunk:` excerpt `pr` carries for that comment ID. A nil `pr` is exactly `BuildPrompt`. |
| `CommentLine(comment) int` | Prefers the new-side line, falls back to the old-side line, returns 0 for file-level comments so callers can omit the line entirely. |
| `IDsOf(comments) []string` | Projects a comment slice into its ID list, preserving order. Returns an empty (non-nil) slice for the empty input. |

## Design notes

- The pure helpers take `store.DiffReviewComment` directly. The
  store type is the shared shape, and re-mapping it locally would add
  conversion without buying clarity.
- The prompt-rendering rules (omit `side`, omit `selectedText`,
  prefer new-side line, two-newline separator) live here so the
  contract is captured in one place and tested directly. For a
  workspace diff the file:line anchor is enough, because the agent can
  read the file the comment points at.
- **A PR review is the case where it is not enough**, which is the whole
  reason `BuildPromptWithPRContext` exists: those hunks come from the
  fetched PR diff rather than the agent's own workspace, so a line
  number alone can anchor to nothing or to the wrong revision. The
  frontend attaches the excerpt (`utils/prHunkExcerpt.ts`, three lines
  of context) only on the `pr` scope, and it rides through on the
  `store.DiffReviewSourceRef`'s `PR` field. Lookup is per comment ID, so
  a comment with no matching entry, or one whose anchor row was not
  found, renders no `hunk:` block rather than borrowing a neighbour's.
