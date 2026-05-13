# internal/planrevision/

Pure helpers behind the proposed-plan inline-comment revision flow:
rendering selected comments as a prompt block the agent can read, and
projecting a comment slice into its ID list.

Mirrors the shape of `internal/diffreview/`. The App-bound CRUD
methods, the `SendPlanRevisionComments` saga that flips a thread
back into plan mode and dispatches the revision, the
`appendPlanRevisionCommentsToContent` composer in `app_send.go`, the
selected-text resolver, and the proposed-plan-upsert emitter all
stay in `app_proposed_plans.go`.

## Surface

| Function | Purpose |
|---|---|
| `BuildPrompt(comments) string` | Renders draft comments into the agent-readable block. `<selected_text>\ncomment: <body>`, blank-separated, fully empty entries skipped. The plan body already sits in the thread, so we only emit comments — no need to restate the plan. |
| `IDsOf(comments) []string` | Projects a comment slice into its ID list, preserving order. Returns an empty (non-nil) slice for the empty input. |

## Design notes

- Imports only `agent-overflow/internal/store` (for
  `ProposedPlanComment`) and stdlib.
- The skip-rule (`selectedText == "" && body == ""`) is captured
  here so the contract is explicit and tested directly. The agent
  has the full plan available in-thread, so emitting a "comment:" on
  an empty body would be pure noise.
