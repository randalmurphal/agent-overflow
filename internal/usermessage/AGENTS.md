# `internal/usermessage`

Owns the JSON metadata shape persisted on user-authored `user_text` rows and
the slash-command word parser shared by send and history rendering.

`Meta` contains attachment display data, plan/review source references,
recognized command state, composer-command expansion, and `SendID`.
`Marshal(Input)`, `FromItem`, and `EncodeDraftSource` are the canonical
shape boundary used by send, queue, retry, fork, and draft restoration.
Coordinate JSON field changes with store, triage, App, and frontend consumers.

`provider_item_id` and `provider_parent_uuid` share the JSON object but are
not fields of typed `Meta`. Modify an existing blob with
`MergeProviderIDs`, `MergeProviderItemID`, or the other merge helpers so
unknown fields and correlation IDs survive. Re-marshalling a decoded `Meta`
silently drops them.

An empty attachment `kind` means image for rows written before the kind field
existed; MIME type cannot replace that compatibility rule. `SendID` belongs
on the message row and remains stable through retry and queue promotion.
`CommandWords` recognizes shapes only; registration and expansion policy
belong to `internal/app`, and its frontend twin must change with it.

See [turn lifecycle](../../docs/architecture/turn-lifecycle.md) for acceptance
and queued-send promotion.
