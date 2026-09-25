# SQLite Schema

The executable schema is the squashed baseline in
`internal/store/schema_v1.go` plus the forward-only migrations in
`internal/store/migrate.go` and `internal/store/migration_v*.go`. Those files
win if this summary disagrees. See [sqlite-store.md](sqlite-store.md) for
connection, migration, restore, trigger, and query contracts.

## Data classes

SQLite has three distinct responsibilities:

| Class | Meaning |
|---|---|
| Provider-history cache | Conversation history and derived render data can be rebuilt from provider session files. Restore and retention may replace or remove it under their documented guards. |
| Application records | Projects, settings, workflow records, automation state, drafts, discussions, and usage are durable application data. Their accessors define their lifecycle. They are not evidence that SQLite is an event store. |
| Authoritative state | Identity, accepted-message queues, transfer ownership, remote command acceptance, and notification ownership have no provider-history replacement. Whole-store snapshot restore handles them deliberately; history retention and generic cleanup must not treat them as cache rows. |

The phrase "SQLite is a history cache" applies to provider history. It does not
make every row disposable.

## Provider history

| Tables | Ownership and key constraints |
|---|---|
| `threads`, `turns`, `message_anchors` | Conversation metadata, thread-scoped turns, and provider message correlation. `threads.history_rev` and `history_epoch` invalidate client replicas. `threads.fork_preparing` keeps incomplete forks listed but unavailable until provider wiring is durable. `threads.deleting` marks a thread whose delete has begun: `DeleteThreadPaced` sets it in its first transaction and removes the row in its last, and the app completes a marked thread at boot (`ListPendingThreadDeletes`, partial index `idx_threads_deleting`). Mode `holder` is a hidden thread that keeps rows pointer forks read after the thread that showed them reverted or deleted them ([sqlite-store.md](sqlite-store.md#ownership)); `owned_threads` leaves holders out, and `trg_thread_fork_lineage_release` sets `deleting` on one no fork reads. `fork_source_thread_id`, `fork_cut_turn_index` and `fork_cut_item_index` record a pointer fork's immediate source and cut; `fork_source_title` is the source's title when the fork was made. `threads.newest_turn_error_at` and `newest_turn_error_turn` are the Failed pill's write-time aggregate, maintained by the turn-error triggers, which count the rows a pointer fork inherits; a fork's writers recompute it when which rows it inherits changes. Narrow lifecycle columns such as fork preparation, import provenance, live todo, group membership, and worktree setup state have dedicated writers and are omitted from broad updates. |
| `items` | Mutable timeline overlay keyed by `(thread_id, id)` and ordered by `(thread_id, turn_index, item_index)`. `summary` is the always-loaded raw preview. `parent_id` links nested work; `completion_of` links completion siblings to launches: the ending one, and a background agent's `parked` stops (`status = 'parked'`). `rev` is the owning thread's `history_rev` when the row's read result last changed (imported rows project -1). Item triggers maintain history stamps, subagent anchor cards, payload collection, import guards, and background liveness. |
| `payloads`, `payload_chunks` | Heavy content keyed by `(thread_id, id)`. Metadata and capped preview spans may ride list reads; base data, chunks, and full spans load on demand. Span blobs are versioned render caches. |
| `thread_fork_lineage`, `thread_fork_hidden` | Pointer-fork reads ([sqlite-store.md](sqlite-store.md#pointer-forks)). `thread_fork_lineage` has one row per ancestor a fork reads through, keyed by `(thread_id, depth)`, with the ancestor and the cut for that level; a holder takes a level of its own. `thread_fork_hidden` lists the inherited ids a fork does not show: rows it copied, reverted or deleted, rows fork creation excludes (live background launches, what an item cut leaves out, and what hangs off either), rows an ancestor added or moved below the cut later, and for a holder the ids it holds, so a row the thread it took them from writes later under one of those ids stays out of the forks' history. Both cascade with the fork's thread row. |
| `payload_snapshots`, `payload_snapshot_refs`, `payload_snapshot_chunks`, `payload_snapshot_edits` | Retired. Migration v120 copied every borrowed payload graph back into the payload rows that referenced it and emptied the tables; nothing reads or writes them. |
| `import_history_chunks`, `import_history_items`, `import_history_payloads` | Immutable imported history. Imports use content-addressed chunks. Chunk-local composite keys keep item and payload identity together. Chunks with `sealed:` ids hold local rows the removed background sealing moved; the history repair folds them back ([sqlite-store.md](sqlite-store.md#history-repair)). |
| `thread_import_chunks`, `thread_import_item_overrides`, `thread_import_state` | Ordered mapping of chunks into a thread, explicit mutable-overlay hides, and provider refresh provenance. Triggers reject gaps, overlaps, and implicit shadowing. Each reference copies its chunk's turn range on attach, and `trg_import_history_items_turn_range` keeps a chunk's range bounding its rows, so a turn read may skip every reference whose range excludes the turn. |
| `edit_file_snapshots` | Gzip-compressed new-side file snapshots for diff expansion. They cascade with their payload and remain cache content: readers verify them against the requested patch. |
| `subagent_aggregates` | One row per local subagent anchor keyed `(thread_id, item_id)`, cascading with its item: the card and tray values a read serves, the positions the incremental rules need, and `state` (clean, dirty, readTime). A local item projection merges a clean row's public values into the served `meta` (`subagentServedMetaSQL`); stored `items.meta` never holds them. `idx_subagent_aggregates_dirty` finds the rows a recompute owes. |
| `subagent_aggregate_backfill` | The threads whose subagent anchors predate the write-time cards. Migration v121's deferred phase stamps them in paced batches and deletes a thread's row with its last unstamped anchor; until then reads walk that thread's unstamped anchors. |
| `agent_end_backfill` | The threads that held an open row under a parent when migration v124 ran, with that time (`opened_at`). v124's deferred phase settles the open rows of each of a thread's ended agents by the live path's agent end rule, one agent per paced write, and deletes the thread's row once none is left to settle ([turn-lifecycle.md](turn-lifecycle.md#agent-owned-rows)). |
| `pending_background_task_terminals` | Claude terminal observations awaiting their chat-side completion sibling. Lifecycle gates consult these rows; tray display retains the launch until the sibling lands. |
| `provider_thread_cost` | Provider-reported cumulative cost estimate for the current `session_ref`. It is replaced in place and ignored when the thread points at a different provider session. It is not a per-turn `usage_ledger` entry. |

`timeline_items` and `timeline_payloads` are logical views over the mutable
overlay, immutable import chunks and a pointer fork's inherited rows.
`timeline_payload_chunks`, `timeline_edit_file_snapshots` and `timeline_turns`
add the inherited append chunks, edit snapshots and turn rows. Ordered, limited,
and recursive item reads use the physical arms from `timeline_arms.go`; see
[sqlite-store.md](sqlite-store.md#logical-history).

`thread_search_rows`, `thread_search`, `thread_search_build` are the agent
search index. `thread_search` is a contentless FTS5 table, so no message text is
stored twice; `thread_search_rows` maps each FTS rowid to its thread, item and
arm (`item` or `import`), and snippets are produced in Go. Settled message text,
tool-call summaries and thread titles are indexed by the write that settles
them; streaming rows are not. Hidden modes are excluded at query time by joining
`owned_threads`, so a mode change needs no reindex. `thread_search_build` holds
one progress row while the background build walks the existing corpus; restore
drops all three and inserts a fresh progress row, because the index is derived
and FTS5 shadow tables cannot be copied row by row.

`owned_threads` is the ownership-filtered thread view. It applies the latest
non-canceled transfer epoch so catalogs and execution checks do not recover a
conversation whose ownership moved to another computer, and it leaves out a
thread whose delete has begun (`threads.deleting`) and a holder, so listings,
search, transfers and fork admission never reach a partly deleted thread or
the kept history of a deleted one.

## Application records

| Tables | Ownership and key constraints |
|---|---|
| `projects` | User-defined repository grouping. `path` and immutable filesystem-safe `slug` are unique. `worktree_setup` is strict JSON owned by `internal/worktreesetup`. `remote_url` and `root_commit` are derived repository identity. Legacy workflow queue columns remain physically present but have no readers or writers. |
| `thread_groups` | Named per-project sidebar groups. Membership is `threads.group_id`; deleting a group ungroups its threads. A grouped thread cannot also carry its own pin. A name is unique per project, case-insensitively, so resolve-or-create by name has one answer. |
| `thread_drafts`, `thread_tracked_files`, `new_thread_mcp_defaults` | Composer drafts, per-thread tracked-file state, and defaults applied to newly materialized threads. Each uses narrow accessors rather than the broad thread projection. |
| `channels`, `channel_messages`, `discussion_definitions` | Multi-agent discussion channels, ordered messages, and reusable global or project templates. |
| `attachments`, `attachment_owners` | Attachment metadata and thread ownership edges; bytes live under `internal/attachment`. The original `attachments.thread_id` names storage provenance and survives deletion of that thread while other owners remain. A pointer fork reads an ancestor's attachment while it shows a row referencing it and owns the ones on rows it copied. `kind` is the closed `image` or `file` vocabulary enforced by `InsertAttachment`. |
| `proposed_plans`, `proposed_plan_comments` | Plan version and inline-review state projected into timeline item metadata. Their mutators bump the owning thread's history revision. |
| `diff_review_comments` | Review comments keyed to diff scope and location. |
| `chat_bar_favorites`, `chat_model_profiles` | Legacy favorite seeds and last-used provider/model settings. Profile constraints remain aligned with thread runtime and reasoning settings. |
| `usage_ledger` | Append-only per-turn, per-model token and cost deltas. Deliberately denormalized without thread or project foreign keys so retained totals survive deletion. Any slice is safe to sum. Token-only rows are priced at read time from `internal/usagecost` by their UTC day. |
| `usage_pending` | Reported token snapshots awaiting final accounting, keyed by thread, provider process scope, segment and model. Survives interruption, restart and thread deletion. Final deltas consume matching pending tokens atomically and retire the settled turn's remaining rows; `usage_records` unions the rest with the settled ledger for queries. Pending rows carry no wire cost and are estimated like token-only rows. |
| `work_items`, `work_item_phases`, `work_item_units` | Durable workflow run, attempt, and fan-out records. State-machine and scheduling rules remain in `internal/workflow`; the store enforces structural relationships and atomic transitions. |
| `work_item_effects` | Idempotency ledger for first-party workflow side effects, unique by run, phase, tool, and payload hash. |
| `workflow_provider_usage_scopes`, `workflow_provider_usage_attention` | Durable attribution and notification ownership for provider-usage parks. They do not decide provider admission. |
| `automations`, `automation_cursors` | Automation definitions, fire receipts, and source watermarks. Fire bookkeeping does not change definition `updated_at`. |
| `scratch_threads` | Where an ephemeral `scratch` fork came from: source thread, the mode a Keep promotion returns it to, and the request it answers. The thread itself carries only mode `scratch`, which is hidden and refused by `threadmode.ValidateSet` in both directions; `PromoteScratchThread` is the only exit. Deleting the thread cascades the row. |
| `ui_state` | Opaque user/device settings plus legacy frontend-state migration buckets. `internal/settings` owns key meaning and scope derivation. |
| `push_tokens`, `push_sender` | Push destinations and sender credentials used by remote notification delivery. |
| `store_meta` | One row containing stable `backend_id` and history-lineage `replica_generation`. Restore preserves the former and remints the latter. |
| `deferred_migration_failures` | One row per migration whose deferred phase last ran with failed items: count, first error and time. Read only above the `PRAGMA user_version` watermark; a clean run clears it. Restore takes the snapshot's rows with the snapshot's watermark ([sqlite-store.md](sqlite-store.md#deferred-phases)). |

`thread_draft_recoveries` holds one staged replacement per thread until a user row
owns its SendID or recovery merges it into the composer. It is durable application
data, independent of history retention and editable drafts. Boot restores unsent
content without dispatching it. Thread deletion cascades to the recovery row.

## Authoritative identity and access

These rows cannot be recovered from provider sessions and are excluded from
provider-history reconstruction. A whole-store snapshot includes them.

| Tables | Ownership and key constraints |
|---|---|
| `users` | Accounts. A partial unique index allows at most one owner. Disabled state is durable authority. |
| `devices` | Client instances and proof-of-possession identity. Key thumbprints, passkey credential references, and nonempty local channels are uniquely indexed. Device and session liveness are evaluated together. |
| `sessions` | Device-to-user grants with JSON scopes, binding class, revocation, and activation. Paired sessions have timed access and signed claims. Local sessions have NULL expiry and a process-bound credential; `idx_sessions_local` permits one unrevoked local session per device. |
| `signing_keys` | HMAC claim-signing secrets. Older keys remain while credentials minted under them may be valid. |
| `recovery_codes` | Hashed single-use recovery credentials. A conditional `UPDATE ... RETURNING` makes consumption atomic. |
| `auth_audit` | Bounded append-only authentication audit. Attribution deliberately has no foreign keys so it can outlive deleted credentials. An update trigger rejects mutation. |
| `pairing_links` | Hashed single-use invitations plus the exact grant, proof, purpose, membership generation, and redemption outcome. |
| `refresh_secrets` | Hashed rotating renewal chain. Spent rows remain until expiry because reuse revokes the session family. |
| `passkeys` | WebAuthn credentials owned by an account, with unique credential ID, public key, relying-party identity, counters, and authenticator flags. |
| `own_devices`, `own_device_sessions` | Personal device membership, removal tombstones, generations, and admitted-session attribution. Membership replacement and session revocation commit together. |

Identity policy, proof validation, claim signing, and scope meaning belong to
`internal/identity`. The store owns durable rows, uniqueness, consumption, and
atomic revocation.

## Authoritative queues and coordination

| Tables | Ownership and key constraints |
|---|---|
| `async_questions` | Durable question and answer state keyed by thread, prompt item, and question index. Prompt text and options survive history-cache pruning. Submission claims questions and queues their user message atomically; user-item insert/meta-update triggers mark echoed answers delivered and retire their recovery queue rows. Restoration commits the draft, question state, and queue removal atomically. Pending questions have no expiry. Explicit history cuts remove prompts or reopen removed answers; forks and transfers carry their surviving state. |
| `flush_queue_items` | Messages accepted by the UI while a turn blocks dispatch. The row may be the only durable copy. Successful provider dispatch or restoration into the composer removes it. Boot restores remaining rows to drafts and does not redispatch them. `send_id` supports idempotency but is empty for internal injection. |
| `thread_transfers` | Move/copy journal, ownership epoch, sealed-manifest identity, retries, cancellation, and cleanup status. It intentionally has no thread foreign key so history deletion cannot erase ownership. Pending incoming rows reserve their project. |
| `thread_transfer_sessions` | Native provider-session closure reserved by a transfer. The latest non-canceled reservation fences execution and import independently of cached AO history. |
| `remote_jobs` | Destination-side command acceptance and bounded receipt, including a `warning` for background processes the command left behind. Request ID and immutable fingerprint prevent a delayed retry from executing twice. Output retention may clear old tails but keeps acceptance and provenance. Boot interrupts unfinished jobs and never replays them. |
| `thread_requests` | Source-side record of one agent thread tool call: token identity, target thread or computer, state, the whole settled answer, a late reply, monotonic revision, and how the answer reached the caller. Every state change is a conditional `UPDATE ... WHERE state = ?` that reports whether it applied; a no-op is a lost race, not an error. Partial indexes serve the remote poller, the undelivered-wake retry and the reminder clock, so their predicates appear explicitly in their queries. `polling` is the poller's gate and part of `idx_thread_requests_poll`, so a settled request with nothing left to report leaves the due set instead of costing a call every few seconds until retention; `wake_attempts`, `wake_next_check` and `wake_issue` pace and bound the retry of a wake the settlement could not hand over; `target_thread_title` carries the answering thread's name when that thread is on another computer and has no row here. |
| `thread_request_receipts` | Destination-side record for the same token, keyed by it so a retried delivery finds the acceptance it already made and spawns nothing. `target_thread_id` is NULL for a spawn or ask until the thread or scratch fork exists, and is named once afterwards; the boot sweep settles an untargeted acceptance like any other open receipt. It binds settlement to the turn that consumed the request's message, and holds the answer of record until the source acknowledges the revision. An uncollected answer expires after a day; the row stays until the retention floor so the token keeps answering. |
| `remote_watches` | Source-side monitoring and notification ownership, with the job's `label` and its `command` display text. Registration precedes the network call. Terminal observations are monotonic. Queueing a completion and inserting its `flush_queue_items` row is one transaction. |

`RestoreFrom` refuses replacement while active commands or transfer phases make
it unsafe. It preserves the live transfer journal, reserved transfer sessions,
remote command receipts, remote watches, and thread requests and receipts
instead of replacing them from the snapshot. It also rejects snapshots that predate current incoming transfer
ownership.

## Important indexes

Most indexes follow directly from an accessor's filter and ordering. The
following families carry additional correctness or performance meaning:

| Index family | Contract |
|---|---|
| Timeline ordering and import indexes | `idx_items_thread_turn_item_unique` enforces one mutable row per timeline coordinate. Import indexes and triggers keep chunk order and identities unambiguous. `idx_thread_import_chunks_turns` ranges a thread's references by turn. |
| Key-first imported lookup indexes | `idx_import_history_items_id`, `idx_import_history_payloads_id` and the imported parent, completion, task, send-id and transcript-root lookup indexes lead with the key and end with `chunk_id`. A lookup reads the matching rows and checks each chunk's membership in the thread, whatever number of chunks the thread references. The ID indexes also serve the chunk-admission collision probes, including chunks sharing a turn. |
| Attachment ownership index | `idx_attachment_owners_attachment` supports last-owner collection and retained-file lookup. |
| Pointer-fork indexes | `idx_thread_fork_lineage_ancestor` finds the forks that read a written row by its position, so a source write past every cut probes no fork, and gives a delete the latest cut of the forks that read the thread. `idx_items_unsettled` gives fork creation the source's running and streaming rows. `idx_items_attachment_refs` and `idx_import_history_items_attachment_refs` hold only rows whose meta lists attachments, so a fork's ownership check reads those rows, not every inherited row; their `instr` predicate must appear in the query. |
| Sparse send identity indexes | Local items, imported items, and queued messages index nonempty `sendId` values so retry checks do not scan or hydrate history. |
| Scoped timeline indexes | Parent indexes include timeline coordinates for ordered child pages. `idx_import_history_items_parent_lookup` starts recursive imported-child lookups by parent identity before checking chunk membership. `idx_items_scope_lifecycle` and its imported counterpart select the latest resume carrier without scanning a transcript. |
| Turn-error indexes | `idx_items_thread_error` and `idx_import_history_items_error` hold only `error` rows, so the turn-error triggers recompute a thread's pair with one probe per arm and per attached chunk. |
| Subagent aggregate indexes | `idx_items_subagent_resume_prompt` finds the resume prompts under an anchor: the rounds a card or a recompute reads, and the prompt a carrier stored after it names (`subagentPromptNamesSQL`). `idx_subagent_aggregates_dirty` finds a thread's dirty anchors by key for `RecomputeSubagentAggregates`. `idx_items_running_nested_fg_tool_calls` holds the nested foreground calls in flight, which `ListLiveBackgroundTasks` walks up to their background launch instead of walking every launch's subtree down. |
| Item relationship partial indexes | Parent, completion, live-background, running-foreground, and reader-authored-message indexes require their qualifying predicate to appear explicitly in query SQL. |
| Workflow relationship indexes | Agent source references are unique; parent-run, phase-thread, unit-thread, automation, state, and usage indexes bound recovery and budget queries. Partial predicates such as `parent_item_id <> ''` and `source_ref <> ''` remain explicit. |
| Credential lookup indexes | Owner uniqueness, credential hashes, live sessions, passkey IDs, device proofs, channels, and refresh families make authorization and single-use consumption indexed atomic operations. |
| Coordination indexes | Transfer ownership/retry, active and settled remote jobs, pending watches, and thread watch discovery bound recovery work independently of conversation size. |

When index selection is part of behavior, tests assert both result parity and
`EXPLAIN QUERY PLAN`. See [sqlite-store.md](sqlite-store.md#query-plans).

## Triggers

| Family | Purpose |
|---|---|
| History revision | Three `items` triggers maintain `threads.history_rev`, `threads.history_epoch`, and the per-row `items.rev` stamp on every row whose read result the write changed: the row, its completion sibling, and the anchors decorated from its parent chain (`stampedRowIDsSQL`: a recursive CTE walks the chain by primary key; the carrier leg probes the partial expression index `idx_items_transcript_root` by root id). They do no subagent card work: the store keeps the cards in Go (`subagent_card.go`). Only the row stamp writes `rev`. The update trigger fires on every column but `rev`, so a stamp does not re-bump the thread. |
| Subagent stamp | Two `subagent_aggregates` triggers stamp the anchor a row write changed, and the completion siblings that borrow its card, with the thread's `history_rev`. `RestoreFrom` drops them for the row copy and reinstalls them. |
| Turn error | Triggers on `items`, `turns`, `thread_import_chunks` and `thread_import_item_overrides` keep `threads.newest_turn_error_at` and `newest_turn_error_turn`: the newest `created_at` and `turn_index` of the visible `error` rows at or after the newest turn (`thread_turn_error_aggregate.go`). `hasFailedTurn` and `MarkThreadReadNow` read the pair, so a thread-row read does no timeline probe. The chunk-detach trigger runs before the delete, while the chunk's rows exist. |
| Payload collection | Item deletion removes payloads no longer referenced by either payload field in the same thread. Cascades collect payload chunks and edit snapshots. |
| Imported-history integrity | Triggers reject implicit shadowing, coordinate overlap, chunk gaps, and ambiguous payload identity; the final thread reference collects immutable storage. |
| Pointer forks | `forkTriggersSQL`: refuse a write that changes a row, payload, append chunk or turn row a fork shows, hide rows an ancestor inserts or moves below a fork's cut, keep a fork's own rows at or after its cut, refuse to delete a thread forks read, mark a holder no fork reads for deletion, and revive a launch whose ending completion a holder takes ([sqlite-store.md](sqlite-store.md#triggers-and-stamps)). |
| Attachment ownership | Initial insertion creates the origin owner. Removing the final owner collects metadata; the attachment package releases file bytes. |
| Background settlement | Four triggers maintain `meta.live_background_active` as launches and ending completion siblings arrive, change, or are removed; a `parked` sibling settles nothing. |
| Authentication audit | A before-update trigger makes `auth_audit` append-only. |

Do not reproduce these invariants in Go write paths. Restore temporarily removes
the item-derived trigger sets for its bulk copy and recreates them from the
shared latest-schema constants.

## Migration policy

Migrations are numbered, forward-only, and append-only. Never edit a migration
that may have shipped. Add a new migration, preserve all current columns,
indexes, triggers, and child relationships in rebuilds, and add a test that
proves the resulting constraint behavior. Detailed rules live in
[sqlite-store.md](sqlite-store.md#migration-model).
