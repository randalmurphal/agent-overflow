# `internal/diffreview`

Pure formatting helpers for draft diff-review comments. App code owns comment
CRUD, store access, and dispatching the follow-up message.

`BuildPrompt` emits non-empty comments in caller order with a file and preferred
new-side or old-side line anchor. `BuildPromptWithPRContext` may add PR identity
and a hunk excerpt keyed by comment id. Keep `IDsOf` order-preserving.
