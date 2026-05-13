# internal/composerdraft/

Builds the composer-draft row a thread should be restored to when a
stored `user_text` item is rehydrated into the composer. Three flows
go through this package:

- Revert-to-message: when the user reverts a thread to before a prior
  prompt, the prompt's text + attachments are projected back into the
  draft slot so the composer rehydrates ready to edit/resend.
- Fork-and-revert (in `app_thread_fork.go`): the
  attachment IDs need to be cloned across thread namespaces before
  building, so the App wraps `FromParts` after running its
  `cloneUserMessageAttachmentsForDraft` helper.
- Flush-queue dispatch: a queued user message that ends up being
  flushed (e.g. on session interrupt) reverts to a draft via the same
  shape.

## Surface

| Symbol | Purpose |
|---|---|
| `FromUserItem(targetThreadID, userItem, updatedAt) (store.ThreadDraft, error)` | Builds a `ThreadDraft` from a `store.Item` directly. Use when the source item's attachment IDs are valid for the target thread (same thread). |
| `FromParts(targetThreadID, content, attachmentIDs, sourcePlan, updatedAt) (store.ThreadDraft, error)` | Builds a `ThreadDraft` from pre-resolved parts. Use when the attachment IDs were re-keyed by the caller (cross-thread fork-and-revert). |

Both helpers populate `TerminalChips: "[]"` deliberately — terminal
chips are composer-only context and not part of the persisted user
item; restoring them from the source item would be incorrect.

## Responsibility boundary

- What BELONGS here: pure projection of `store.Item` → `store.ThreadDraft`
  via the existing `usermessage.FromItem` decode + JSON re-encoding.
- What does NOT belong here: cross-thread attachment cloning (needs
  `a.attachments` from the App) and the binding-tier types (`Draft`,
  `TerminalChip`) — those stay in `app_draft.go` because the Wails
  binding generator emits TS types from the main-package shapes.

## Anti-patterns

- Do NOT inline the wire shape here. Wails bindings must stay in the
  main package; if the wire shape ever needs to change, the App layer
  marshals back and forth, not this package.
- Do NOT silently smuggle `TerminalChips` from anywhere; the empty `[]`
  is part of the contract.
