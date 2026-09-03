# internal/usermessage/

Owns the JSON wire shape persisted in `store.Item.Meta` for
user-authored timeline rows (`user_text`), plus the marshal /
unmarshal / draft-source-encode helpers that every entry point
funnels through: send, steer, flush-queue dispatch, fork-and-revert,
and the composer-restore-from-user-item path.

The frontend reads this blob back to render the user row's
attachment chips, source-plan badge, and revision-context badges, so
the JSON tags are part of the contract. App-bound saga code in
`internal/app/app_send.go` / `app_draft.go` / `app_flush_queue_restore.go` /
`app_codex_provider_queue.go` / `app_steer.go` builds inputs out of `store.Attachment`,
`store.ProposedPlanSourceRef`, and `store.DiffReviewSourceRef`,
then calls the package's `Marshal` / `FromItem` to cross the
serialisation boundary.

## Wire-shape contracts

| Symbol | Purpose |
|---|---|
| `Meta` | Top-level wire shape persisted in `store.Item.Meta`. All fields are `omitempty` so a zero meta serialises to `""`. |
| `AttachmentMeta` | Per-attachment slice element. The frontend renders these alongside the user row; storage-internal columns (path, hashes, timestamps) deliberately do not appear. `kind` (`image` / `file`, `omitempty`) is what the row renders as, and EMPTY MEANS IMAGE — every row written before the kind existed carried one, so an absent value is the truth about that row, never a gap to repair. The MIME type cannot stand in for it: `image/heic` is a `file`, because no provider ingests one. |
| `Input` | The per-entry-point projection `Marshal` encodes. A struct, not a positional list: several fields share a type, so a swapped pair used to compile clean. |
| `Marshal(in Input) (string, error)` | Builds the JSON blob from the per-entry-point inputs. Returns `("", nil)` when every input is zero-valued so callers can persist an empty Meta column. |
| `CommandWords(content) []string` | Every command-shaped word in a message, in order of appearance and without its slash (D31). Any word position counts, duplicates are preserved. Shape only: whether a name is REGISTERED is the caller's question, and the table lives in `internal/app`. |
| `FromItem(item store.Item) (Meta, error)` | Decodes the persisted blob back into a `Meta`. Empty / whitespace-only Meta decodes to the zero `Meta` with no error. |
| `EncodeDraftSource(ref *store.ProposedPlanSourceRef) (string, error)` | JSON-encodes the draft's `PendingPlanImplementation` column. Returns `("", nil)` for a nil ref or an empty ItemID so the column stores SQL NULL and the partial index `idx_thread_drafts_pending_plan_impl` stays selective. |

## Design notes

- **Wire-correlation ids ride OUTSIDE the typed `Meta`.**
  `provider_item_id` and `provider_parent_uuid` are top-level JSON keys
  on the same blob, internal correlation rather than UI content, read
  with `ReadProviderItemID` / `ReadProviderParentUUID` and written with
  `MergeProviderIDs` (or `MergeProviderItemID` / `MergeCommand`). Update
  a stored blob by MERGING: those helpers decode to a map and preserve
  every key, while re-marshaling a decoded `Meta` silently drops the
  correlation ids and breaks echo matching and revert slicing. The echo
  stamp writes both ids in one encode so a failed follow-up write cannot
  lose the parent uuid.
- Imports only `agent-overflow/internal/store` and stdlib.
- Hand-written tests cover the omit-empty contract, the round-trip
  through `FromItem(Marshal(...))`, and the empty-ItemID short-circuit
  on `EncodeDraftSource`, the three places the frontend's behaviour
  depends on this package matching the contract.
- `Meta.Command` records which composer slash command expanded on a send
  (D31). The stored `Summary` is what the user typed; the expansion
  exists only in the provider payload, so this marker is the only record
  that one happened, and chat history colours every occurrence of that
  word from the marker rather than from a live registry match, so a row
  cannot start or stop claiming an expansion when the registry changes.
  The parse that decides what a command word IS lives here (`command.go`)
  because both the send path and history rendering must agree on it. A
  command named more than once expands once and is marked once; the
  repetition is a rendering fact, not a payload one.
- The composer-draft builders that consume `Meta`
  (`composerDraftFromUserItem*` in `internal/app/app_draft.go`) stay in `internal/app`
  because they bind `a.attachments` for cross-thread cloning.
