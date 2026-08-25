# internal/usermessage/

Owns the JSON wire shape persisted in `store.Item.Meta` for
user-authored timeline rows (`user_text`), plus the marshal /
unmarshal / draft-source-encode helpers that every entry point
funnels through: send, steer, flush-queue dispatch, fork-and-revert,
and the composer-restore-from-user-item path.

The frontend reads this blob back to render the user row's
attachment chips, source-plan badge, and revision-context badges —
the JSON tags are part of the contract. App-bound saga code in
`app_send.go` / `app_draft.go` / `app_flush_queue_restore.go` /
`app_codex_provider_queue.go` / `app_steer.go` builds inputs out of `store.Attachment`,
`store.ProposedPlanSourceRef`, and `store.DiffReviewSourceRef`,
then calls the package's `Marshal` / `FromItem` to cross the
serialisation boundary.

## Surface

| Symbol | Purpose |
|---|---|
| `Meta` | Top-level wire shape persisted in `store.Item.Meta`. All fields are `omitempty` so a zero meta serialises to `""`. |
| `AttachmentMeta` | Per-attachment slice element. The frontend renders these alongside the user row; storage-internal columns (path, hashes, timestamps) deliberately do not appear. |
| `Input` | The per-entry-point projection `Marshal` encodes. A struct, not a positional list: several fields share a type, so a swapped pair used to compile clean. |
| `Marshal(in Input) (string, error)` | Builds the JSON blob from the per-entry-point inputs. Returns `("", nil)` when every input is zero-valued so callers can persist an empty Meta column. |
| `CommandWords(content) []string` | Every command-shaped word in a message, in order of appearance and without its slash (D31). Any word position counts, duplicates are preserved. Shape only — whether a name is REGISTERED is the caller's question, and the table lives in the main package. |
| `FromItem(item store.Item) (Meta, error)` | Decodes the persisted blob back into a `Meta`. Empty / whitespace-only Meta decodes to the zero `Meta` with no error. |
| `EncodeDraftSource(ref *store.ProposedPlanSourceRef) (string, error)` | JSON-encodes the draft's `PendingPlanImplementation` column. Returns `("", nil)` for a nil ref or an empty ItemID so the column stores SQL NULL and the partial index `idx_thread_drafts_pending_plan_impl` stays selective. |

## Design notes

- Imports only `agent-overflow/internal/store` and stdlib.
- Hand-written tests cover the omit-empty contract, the round-trip
  through `FromItem(Marshal(...))`, and the empty-ItemID short-circuit
  on `EncodeDraftSource` — the three places the frontend's behaviour
  depends on this package matching the contract.
- `Meta.Command` records which composer slash command expanded on a send
  (D31). The stored `Summary` is what the user typed; the expansion
  exists only in the provider payload, so this marker is the only record
  that one happened — and chat history colours every occurrence of that
  word from the marker rather than from a live registry match, so a row
  cannot start or stop claiming an expansion when the registry changes.
  The parse that decides what a command word IS lives here (`command.go`)
  because both the send path and history rendering must agree on it. A
  command named more than once expands once and is marked once; the
  repetition is a rendering fact, not a payload one.
- The composer-draft builders that consume `Meta`
  (`composerDraftFromUserItem*` in `app_draft.go`) stay in main
  because they bind `a.attachments` for cross-thread cloning.
