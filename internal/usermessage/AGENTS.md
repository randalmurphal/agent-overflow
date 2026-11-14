# `internal/usermessage`

Owns the JSON metadata shape persisted on user-authored `user_text` rows and
the slash-command word parser shared by send and history rendering.

`Meta` contains attachment display data, plan/review source references,
recognized command state, composer-command expansion, `SendID`, and the
text before the review comments a message rendered
(`TextBeforeReviewComments`): a message taken back undelivered restores
that text to the composer, and its comments return to drafts.
`Marshal(Input)`, `FromItem`, and `EncodeDraftSource` are the canonical
shape boundary used by send, queue, retry, fork, and draft restoration.
Coordinate JSON field changes with store, triage, App, and frontend consumers.

`provider_item_id` and `provider_parent_uuid` share the JSON object but are
not fields of typed `Meta`. Modify an existing blob with
`MergeProviderIDs`, `MergeProviderItemID`, or the other merge helpers so
unknown fields and correlation IDs survive. Re-marshalling a decoded `Meta`
silently drops them.

An attachment's `Shell` (`ShellMeta`) is set when the file is the output of
a composer shell command. Every string in it is bounded
(`ShellCommandMetaBytes`, the attachment's preview) so the item wire never
elides one; the attachment record keeps the whole command, and the row's
text keeps the typed `!command`. A bounded command carries its whole
length (`CommandLength`, in UTF-16 code units) so the frontend can still
tell where its invocation ends.

`shell_invocation.go` owns which parts of a message stand for which shell
attachment (`ShellInvocationRuns`), as `command.go` owns the slash command
word. The send path replaces those parts with the provider's command block
and the timeline hides them behind the block. The frontend's copy is
`utils/shellInvocation.ts` (`MESSAGE_JOIN_SEPARATOR` mirrors
`JoinSeparator`). Both run `testdata/shell_invocation_vectors.json`, kept
byte-identical with `frontend/src/test/fixtures/shellInvocationVectors.json`:
change the rule by changing the vectors and both implementations.

An empty attachment `kind` means image for rows written before the kind field
existed; MIME type cannot replace that compatibility rule. `SendID` belongs
on the message row and remains stable through retry and queue promotion.
`CommandWords` recognizes shapes only; registration and expansion policy
belong to `internal/app`, and its frontend twin must change with it.

See [turn lifecycle](../../docs/architecture/turn-lifecycle.md) for acceptance
and queued-send promotion.
