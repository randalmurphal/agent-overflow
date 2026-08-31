# internal/composerdraft/

Builds the composer-draft row a thread should be restored to when a
stored `user_text` item is rehydrated into the composer. Four flows go
through this package:

- Revert-to-message: when the user reverts a thread to before a prior
  prompt, the prompt's text + attachments are projected back into the
  draft slot so the composer rehydrates ready to edit/resend.
- Fork-and-revert (in `app_thread_fork.go`): the
  attachment IDs need to be cloned across thread namespaces before
  building, so the App wraps `FromParts` after running its
  `cloneUserMessageAttachmentsForDraft` helper.
- Flush-queue restore: queued user messages that never reached the
  provider (session death, failed resend) merge back into whatever the
  composer already holds.
- Edit-and-resend (`app_revert_and_resend.go`): the same merge, staged
  so the saga can settle it back if the resend fails.

## Which builder to call

| Symbol | Use when |
|---|---|
| `FromUserItem(targetThreadID, userItem, updatedAt)` | The source item's attachment IDs are already valid for the target thread (same thread). |
| `FromParts(targetThreadID, content, attachmentIDs, sourcePlan, updatedAt)` | The caller re-keyed the attachment IDs (cross-thread fork-and-revert). |
| `MergeParts(targetThreadID, current, parts, updatedAt)` | Something must be ADDED to a draft the user may already be typing in. Never overwrite that row with a `From*` result. |

`MergeParts` takes `[]Part` (the same content / attachment-ID /
source-plan triple `PartFromUserItem` projects out of a stored item) and
folds it into `current` under fixed rules: restored parts first in
caller order and the existing draft content LAST, blank-line separated,
because the restored messages were typed before whatever is sitting in
the composer now. Attachment IDs dedupe on first occurrence. An existing
pending plan implementation wins; only when the draft has none does a
source plan common to every restored part carry through.

Both `From*` builders hard-set `TerminalChips: "[]"`, because terminal
chips are composer-only context, not part of the persisted user item.
`MergeParts` is the opposite and deliberately so: it carries the CURRENT
row's chips through untouched (defaulting to `"[]"` only when the row
has none), since that draft is the user's live composer and a merge must
not cost them context they staged themselves.

## Responsibility boundary

- What BELONGS here: pure projection of `store.Item` → `store.ThreadDraft`
  via the existing `usermessage.FromItem` decode + JSON re-encoding.
- What does NOT belong here: cross-thread attachment cloning (needs
  `a.attachments` from the App) and the binding-tier types (`Draft`,
  `TerminalChip`), which stay in `internal/app/app_draft.go` because the Wails
  binding generator emits TS types from the application-shell shapes.

## Anti-patterns

- Do NOT inline the wire shape here. Wails bindings must stay in
  `internal/app`; if the wire shape ever needs to change, the App layer
  marshals back and forth, not this package.
- Do NOT let a `From*` builder carry `TerminalChips` out of a stored
  user item. The empty `[]` is part of that contract, and `MergeParts`
  preserving the live draft's own chips is not a precedent for it.
