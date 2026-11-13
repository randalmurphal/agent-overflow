# `internal/diffreview`

Pure formatting helpers for draft diff-review comments. App code owns comment
CRUD, store access, and dispatching the follow-up message.

`BuildPrompt` emits non-empty comments in caller order with a file and preferred
new-side or old-side line anchor, a range as its first and last line on the
half both share, and a held reply under the thread it answers.
`BuildPromptWithPRContext` may add PR identity, a hunk excerpt and a held
reply's thread (file, lines, comments) keyed by comment id. Keep `IDsOf`
order-preserving.
