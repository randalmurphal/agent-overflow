# Thread Replica Sync: rev/epoch invalidation + IndexedDB replica

Status: SHIPPED 2026-08-10 (wave 2 of the cold-thread-loading work); phase 3,
the remote era, is still deferred. Read this as the design record behind
`frontend/src/lib/replica/` and the `history_rev`/`history_epoch` stamps.

## 1. Problem

A cold thread open needs a server read before its timeline can show current
history. The in-memory `threadItemCache` (LRU of 5 threads) and the
IndexedDB replica can supply that window locally, but the pane verifies
it before display. An unchanged window needs only a small sync answer;
stale rows are replaced before they can appear.

The replica makes that local window durable and its freshness checkable:

- a per-thread **`rev`/`epoch`** stamp pair on the backend that any item
  mutation provably advances (the invalidation contract, §3),
- one RPC, **`SyncThreadWindow`**, that either answers "nothing
  changed" with no items attached, or returns the viewport window (§5),
- an **IndexedDB replica** of recently viewed thread windows, staged before
  the RPC and reconciled before display (§6).

Non-goals for this wave: offline pagination (loadOlder/loadNewer stay
server-only), replicating payload bodies (principle 4: heavy payloads
stay lazy-loaded), replicating turn rows or live state (their RPCs keep
firing on every open), CRDTs, event sourcing, or any change to the
reveal queue. The replica saves transfer and avoids rebuilding an unchanged
window after verification; the
backend store remains the only queryable truth
(`CLAUDE.md` core principles 2–4).

## 2. Why `updated_at` and the event ring cannot do this

Facts established by auditing every history-mutating path in
`internal/store` (2026-08-08, this branch):

- **No per-thread counter exists.** Item ordering is
  `(turn_index, item_index)` with `item_index` restarting per turn;
  there is no monotonic sequence a client cursor could ride.
- **`threads.updated_at` deliberately undercounts.** Only
  `MarkThreadActivity` and a short allowlist bump it; item upserts,
  streaming summary appends, lifecycle flips, and every payload write
  leave it untouched (`internal/store/threads.go`, doc comment on
  `MarkThreadActivity`).
- **Rows change without moving.** Streaming finalize
  (`ReplacePayloadData`), tool-call completion (`UpsertItem` update
  branch), interrupt force-close (`ForceCloseRunningToolCallsInTurn`),
  Codex runtime retirement (`RecoverCodexBackgroundRuntime`), crash recovery
  (`RecoverCrashedTurns`), and meta merges all rewrite existing rows in
  place. Some (`UpdateItemMeta`, the fork uuid remap) skip `updated_at`
  *by design*.
- **The wire DTO joins mutable payload columns.** `payloadMeta` and
  `payloadPreviewSpans` ride the item row on the wire
  (`internal/store/items.go` `itemColumns`) but live in `payloads`,
  which has no `thread_id` and whose spans are backfilled by an async
  worker (`UpdatePayloadSpans`) with zero signal on the item row.
- **History is cut non-contiguously.** `DeleteConversationFromItem`
  removes a provider-order slice no boundary comparison can express
  (its own doc comment says so); `BumpItemToTurnEnd` repositions a live
  row; `UpsertItemAtTurnHead` inserts *below* existing indices.
- **The transport event ring is a jitter buffer, not history**
  (`internal/transport/AGENTS.md`): replay covers reconnect gaps
  measured in seconds, not "this device was closed for a week". Replica
  reconciliation cannot lean on WS replay.

So the contract must be new state, bumped in the same transaction as
the mutation, and enforced structurally, not by asking every present
and future store function to remember to call a bump helper.

## 3. The invalidation contract

Two `INTEGER NOT NULL DEFAULT 0` columns on `threads` (migration v55):

- **`history_rev`** advances on *every* persisted mutation that can
  change what a windowed item read for that thread returns: item
  insert/update/delete and payload content/meta/span writes.
  `rev` equality means "byte-identical window reads"; it says nothing
  about *what* changed.
- **`history_epoch`** advances on mutations that a client holding a
  cached ordered window cannot survive by re-fetching a range:
  item deletion and item repositioning. Epoch says "your cached
  ordering may show rows that no longer exist or sit elsewhere, so do
  not paint scrollback you haven't re-fetched".

Every epoch bump also bumps rev, so `rev` match alone implies fully
fresh; `epoch` exists to grade *how* stale a mismatch is.

**Rev 0 is unreachable for a thread that predates the contract.** v55's
SQL ends with `UPDATE threads SET history_rev = 1;`. Without it, every
pre-existing thread would sit at `(0, 0)` holding full history, and
`(0, 0)` is *also* the JSON zero value of the sync request's stamp pair,
so a client that omits the fields (or a bug that drops them) asks "is rev
0 still current?" and gets a page-less `fresh` over 400 items it does not
have. After the lift, `(0, 0)` can only describe a thread with no item
writes since v55, whose window is empty and for which a page-less `fresh`
is the truthful answer. The column DEFAULT stays 0 for exactly that
reason: a brand-new thread genuinely is at the origin.

### 3.1 Enforcement: triggers on `items`, thread-scoped payload API

Item-side bumps are SQLite triggers (installed by v55, alongside the
existing `trg_items_gc_*` precedent; replayed drop-then-create by a later
migration when a body changes), so no store function, present or future, can
write an item row without advancing the contract. The same triggers
stamp the per-row revision `items.rev`: the thread's `history_rev` as of
the last write that changed what a read of that row returns. Two reads of
the same `(id, rev)` are byte-identical, which is what lets a client
describe the window it already holds (§5).

```sql
CREATE TRIGGER trg_items_rev_insert AFTER INSERT ON items BEGIN
  UPDATE threads SET history_rev = history_rev + 1
   WHERE id = NEW.thread_id AND history_bulk_load = 0;
  UPDATE items SET rev = (SELECT history_rev FROM threads WHERE id = items.thread_id)
   WHERE thread_id = NEW.thread_id AND id = NEW.id
     AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 1;
  UPDATE items SET rev = (SELECT history_rev FROM threads WHERE id = items.thread_id)
   WHERE thread_id = NEW.thread_id AND id IN (<rows a write to NEW changed>)
     AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 0;
END;

CREATE TRIGGER trg_items_rev_update AFTER UPDATE ON items
WHEN OLD.rev IS NEW.rev
BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch
      + (OLD.turn_index IS NOT NEW.turn_index OR
         OLD.item_index IS NOT NEW.item_index OR
         OLD.thread_id  IS NOT NEW.thread_id)
  WHERE id IN (OLD.thread_id, NEW.thread_id) AND history_bulk_load = 0;
  UPDATE items SET rev = (SELECT history_rev FROM threads WHERE id = items.thread_id)
   WHERE (thread_id = NEW.thread_id AND id IN (<rows a write to NEW changed>))
      OR (thread_id = OLD.thread_id AND id IN (<rows a write to OLD changed>));
END;

CREATE TRIGGER trg_items_rev_delete AFTER DELETE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch + 1
  WHERE id = OLD.thread_id AND history_bulk_load = 0;
  UPDATE items SET rev = (SELECT history_rev FROM threads WHERE id = items.thread_id)
   WHERE thread_id = OLD.thread_id AND id IN (<rows a write to OLD changed>);
END;
```

`rev` is stamped only here. No INSERT or UPDATE column list in Go names
it, so a new writer cannot forget it and cannot lie about it.

The rows a write changed (`stampedRowIDsSQL`) are the rows whose READ
result it changed, not only the row itself, because a page decorates
some top-level rows from other rows (`decorateSubagentAnchors`):

- the written row;
- its completion sibling (`completion_of`), whose card is walked from the
  launch row;
- when the row has a parent: every row on its parent chain (the
  descendant aggregate is transitive, so a write under a nested launch
  changes the outer launch's read too), every resume carrier whose
  `transcript_root_id` names one of those (a carrier's round is counted
  from the root's children, claude-wire.md §E6), and the completion
  siblings of all of those.

Each leg is an index probe (the primary key, walked once per level of
the chain by a recursive CTE, `idx_items_completion_of`, and the v100
partial expression index `idx_items_transcript_root`, keyed on the root
id), so a child write costs a handful of probes per nesting level whatever
the thread's size. The carrier leg compares against `+ancestors.id`: the
CTE column's TEXT affinity would otherwise apply to the indexed
expression and limit the probe to the thread's whole carrier set.
`TestItemRevisionStampProbesIndexes` pins the plan of the query and
`TestItemRevisionTriggersProbeCarriersByValue` the installed triggers. The set is one
`id IN (...)` so an overlapping leg stamps a row once: a second stamp
that left `rev` unchanged would pass the update trigger's guard below and
bump the thread twice. `TestHeldWindowSeesThroughToAnchorsWalkedFromOutside`
holds a window of carriers and completion siblings with the launch
scrolled out of it and proves a child write under the launch, or under
a launch nested inside it, still refuses the window.

Between the thread bump and the row stamp, the same triggers keep each
subagent anchor's card in its `subagent_aggregates` row (migration v121;
the contract is in `internal/store/subagent_aggregate_stamps.go`): the
counts, the preview and tray values, and the positions the incremental
rules need, updated along the written row's parent chain with the same
probes, so a child write rewrites one narrow row per nesting level,
never the anchor's `meta`, and never walks the subtree. A local item
projection merges a clean row's public values into the served `meta`
with `json_patch`; a write to the row stamps the anchor and its
completion siblings, so a changed card is always served at a new
revision. A write no incremental rule keeps exact marks the chain
dirty, and the writer recomputes it (`RecomputeSubagentAggregates`)
before it commits. `decorateSubagentAnchors` serves a clean stamped
anchor as the projection read it and walks only the rows the triggers
do not keep:
imported anchors, dirty and `readTime` rows, carriers whose round prompt
has not arrived, and unstamped anchors of a thread still listed in
`subagent_aggregate_backfill` for v121's deferred phase.
`TestSubagentAggregateStampsMatchTheReadTimeAggregator` compares every
served card with the walk after each kind of write;
`TestSubagentAggregateTriggersDoNotScanASubtree` and
`TestSubagentAggregateStatementPlans` pin the cost.

The update trigger's `WHEN OLD.rev IS NEW.rev` guard is what stops the
nested `UPDATE items` from re-entering the thread bump. `recursive_triggers`
is OFF (pinned in `writerConnPragmas` with boot verification) so a trigger
does not re-fire *itself*, but the insert trigger's stamp is an UPDATE and
would otherwise fire the update trigger, bumping `history_rev` a second
time per insert. The guard is the stamping write's signature: it is the
only item write that leaves every other column alone. Exact arithmetic for
insert, update, delete and child writes is pinned by
`TestItemRevisionTriggerArithmetic`.

Imported history rows (`import_history_items`, shared across threads by
`thread_import_chunks`) have no thread-scoped place to stamp, so the
imported arm of `timeline_items` reads `rev = -1` (`importedItemRevExpr`).
A held window containing one is refused rather than verified (§5).

A chunk-derived stamp does not work in place of -1, because an imported
row's read result changes while its chunk reference stands still. Two
write paths do it today: a payload mutator copies the imported payload
into the thread's local overlay (`ensureLocalPayloadTx`) and the imported
arm hydrates `COALESCE(local_payloads..., imported_payloads...)`, so kind,
meta and preview spans change; and a local child parented to an imported
`tool_call` changes that anchor's decorated meta. Neither write has an
`items` row to stamp. The cost of the refusal is bounded: only a window
that CONTAINS imported rows is refused, so the live tail of an imported
thread still verifies.

Cost: the bump is an extra dirty page in the same transaction; WAL
writes pages per commit, not per statement, so a 500-row retention
chunk delete or a 10 Hz streaming append batch pays one extra page
image per commit. This satisfies the remote-access budget rule that
streaming must not gain per-frame work (§14). The bump rides commits
that already exist.

The row stamp is a second UPDATE of the written row inside the same
transaction, and SQLite rewrites the whole record for it, so its CPU cost
grows with the row. Measured on an in-memory store appending a 36-byte
delta to one `assistant_text` row (`AppendItemSummary`, no stamps vs the
full stamp set): 0.19 ms vs 0.47 ms at a 4 KB summary, 0.40 vs 0.68 ms
at 40 KB, 0.65 vs 1.22 ms at 200 KB. The anchor legs are about 0.09 ms
of that; the rest is the row copy. WAL page images do not grow: the
stamp dirties pages the append already dirtied.

`ApplyImportBatch` is the bulk-load exception without being a contract
exception. Inside its uncommitted transaction it sets the private
`threads.history_bulk_load` flag; the trigger predicates therefore skip their
per-row updates. After inserting the batch, the writer adds exactly the number
of inserted item rows to the revision and clears the flag. Readers cannot
observe it, a rollback restores it automatically, and committed revision
values remain identical to the ordinary trigger path. Rows written under the
flag are still stamped, with the thread's current `history_rev`; because the
aggregate bump lands before commit, `history_rev` ends greater than every rev
stamped inside the batch, so no client can hold a rev a later read would
reproduce for different bytes.

Under the flag the insert trigger stamps only the inserted row. Every
insert into `items` under the flag moves a row a read already showed from
imported history (`localizeImportedItemTx`, `UnsealThreadHistory`) or
rebuilds a thread whose rows the same transaction deleted (a returning
transfer), so no other row's read changes; stamping the inserted row's
anchors would rewrite each of them once per moved child. A writer that
inserts a row no read showed must not hold the flag. The update and delete
triggers stamp the full set under the flag, because thread deletion chunks
delete under it and a delete changes its anchors' reads.
`TestMigrationV119BulkLoadInsertStampsOnlyTheRow` pins both sides of the
insert gate.

Payload-side bumps stay explicit in the payload mutators rather than adding a
second trigger path. Since migration v58 payload rows carry `thread_id`, the
same scope selects the row and advances its owner's revision in one
transaction. The payload mutators that run *outside* an item-row transaction
(`ReplacePayloadData`, `UpdatePayloadMeta`, `UpdatePayloadSpans`, and
any bare `AppendPayloadData`) change signature to take `threadID` and
bump `history_rev` inside their own transaction. The signature is the
enforcement: a future caller cannot reach payload mutation without
naming the thread. (Their callers all know the thread already: triage
stream finalize, `rebuildCommandOutputMeta`, `persistPayloadSpans`.)
For the signature to *be* the enforcement there must be
no way around it, so the bare `InsertPayload` / `UpsertPayload` exports
are removed: a payload is window-visible only through the item that
references it, and the private item-coupled upsert additionally resets
derived chunks, snapshots, and span blobs. Production
payload creation goes through the item-coupled writers
(`InsertItemWithPayload`, `AppendItemWithPayload`, `UpsertItem`).
The item-coupled combos
(`UpsertItemWithPayloadAppend`, `AppendItemSummaryAndPayloadData`) are
covered by the item triggers and need no second bump; double bumps
would be harmless anyway, because the contract is monotonic, never
exact.

A window-visible write outside `items` must move the row revision of the
rows it changes, not only the thread's. Two helpers do that, and they are
the only way those writers bump:

- `bumpHistoryRevForPayloadTx` (`AppendPayloadData`, `ReplacePayloadData`,
  `UpdatePayloadMeta`) issues `UPDATE items SET rev = rev` over the rows
  whose `payload_id` or `input_payload_id` is the payload, each matched
  through its own partial index. The statement changes no column; the
  update trigger does the stamping and the thread bump. When it matches no
  row (the owner is imported history) it falls back to `bumpHistoryRevTx`
  so the thread stamp still invalidates.
- `bumpHistoryRevForItemTx` (proposed-plan state and comment writers) does
  the same for the plan item id, because plan rows are decorated from those
  tables on read (`decorateProposedPlanItems`).

`UpdatePayloadSpans` is excluded on purpose and keeps the bare
`bumpHistoryRevTx`. Spans are a derived cache with a documented "empty means
not computed, ask the highlight RPC" fallback and are version-checked against
payload content on the client (`utils/payloadVersion.ts`), so a window whose
spans are behind is still a correct window.

Every pushed `ItemStreamEvent` is the row a page reads, at the revision
it reads it, or claims no revision (`TestEmittedItemEventsCarryStoredItemRev`),
because a client builds its held window out of the rows it was pushed:

- an upsert of a row whose page read is the stored row sends the row
  read back inside its write transaction; the caller's input struct
  carries a pre-trigger value;
- an upsert of a row whose page read is decorated
  (`store.ItemReadNeedsDecoration`: a completion sibling, a proposed
  plan, or an anchor the read walks: dirty, `readTime`, an unstamped
  carrier, or an unstamped anchor of a thread whose backfill is pending)
  sends the write's read-back marked `store.UnstampedItemRev` and notes
  the row at that revision, so the anchor refresh below pushes its page
  read. The read-back is an altered row (the launch without its
  descendant count) and must not claim the stored revision. The write
  never runs the decorated read itself: it walks the anchor's
  descendants, and the writes under a large agent arrive at tens per
  second on the provider event path. The gate reads the row's own stamp,
  plus one backfill-list probe for an unstamped row, so a plain tool call
  (every tool start and result, every Codex command-output flush) and a
  clean stamped anchor go out stamped;
- a `patch` carries `patch.rev`, the revision `UpdateItemFields` read
  inside the same transaction as the write. Without it every settled row
  would hold the revision its last upsert carried and no window
  containing one could verify. A patch replaces the client's `meta`
  wholesale, so a decorated row is never patched: `persistItemFieldsAndPatch`
  pushes it as an unstamped upsert instead;
- an upsert whose row the emitter altered on purpose carries
  `store.UnstampedItemRev` (-1). The streaming reveal blanks the summary
  so the text can arrive as deltas, and that wire row is not the stored
  row; claiming the stored revision beside altered content is the one way
  to earn a false `fresh`. The settle patch closes the sequence with the
  real revision.

The mid-stream `meta` action still carries no revision. It only reaches
rows that are streaming, and those settle through a patch that does, so a
held row's revision is stale only while its content is visibly in flight.

A write also stamps rows it did not touch (the list above: the parent
anchor, its resume carriers, the completion siblings), and nothing pushes
those. Left there, a client that watched a subagent run would hold every
anchor at a revision behind the store's and pay a page on each reopen,
the cost §1 exists to remove. The router's anchor refresh
(`internal/triage/wire_items.go`) closes it: every upsert and patch notes
its row and pushed revision on the thread, whether or not the thread has
a live session; at a quiet point the rows
those writes stamped (`ListWireItemsBehind`, the trigger's own candidate
select as a query, plus the launch a completion sibling settles) are
read as a page would and pushed again, skipping a written row whose
stored revision still equals its pushed one. Quiet points are one second
after the thread's last push, at most five seconds after its first, turn
completion, and session teardown; the router's drains flush them too. A
refresh is never per write: an anchor push rebuilds the client's
grouping, and a subagent's tool calls arrive at tens per second. A
refresh that fails leaves the client's copies behind, which costs a page,
never a false `fresh`. `TestSubagentTurnLeavesEveryPushedRowProvable`
drives a whole subagent turn and proves the window built from the last
push of each top-level row verifies `fresh`.

One anchor push does not wait for the quiet point: the row that opens an
agent's card (`ListFirstChildWireAnchors`: a clean stamped parent, or the
carrier a resume prompt names, whose round now holds that row alone)
pushes the anchor with it, as stored, so the card appears with the
agent's first activity. The emitter probes once per parent per session
and always for a resume prompt, which can open a carrier under a parent
it has seen; `TestAgentsFirstRowPushesItsCardAtOnce` and
`TestSubagentToolEventReadsItsRowOnce` pin the push and its read cost.

### 3.2 Operation → contract map

| Operation (store) | Trigger path | Contract effect |
|---|---|---|
| `AppendItem`, `UpsertItem` insert, `AppendCompletionItem`, import tail append (`ApplyImportBatch`) | items INSERT | rev |
| `UpsertItemAtTurnHead` insert (below existing indices) | items INSERT | rev: additive; a stale paint merely lacks the row until reconcile |
| Streaming summary appends/tails, `UpsertItem` update, `UpdateItemMeta(-Merge)`, `UpdateItemFields`, lifecycle flips, `RecoverCrashedTurns`, ghost flips | items UPDATE | rev |
| `BumpItemToTurnEnd` (reposition) | items UPDATE (index changed) | **epoch** |
| `DeleteThreadItem`, `DeleteConversationFromTurn`, `DeleteConversationFromItem` | items DELETE | **epoch** |
| `ReplacePayloadData`, `UpdatePayloadMeta`, `UpdatePayloadSpans` (async span backfill), bare `AppendPayloadData` | explicit, new `threadID` param | rev |
| Fork clones (`CloneThreadItems`, `CloneThreadHistoryBeforeItem`) | items INSERT on the *target* thread | rev on target; source untouched |
| Import rollback / `DeleteThread` / retention sweep | thread row deleted | tombstone: replica entry dropped by the deleting client directly, and by any other client on the `gone` answer (§5) |
| `RestoreFrom` (harness snapshot) | whole-DB replace | **generation** re-mint (§3.3) |
| `decorateSubagentAnchors` (stamp read, or the walk for rows the triggers do not keep) | none: no write occurs | covered transitively: its inputs are the anchor's stamp and descendant item rows, whose writes bump rev |
| `RecomputeSubagentAggregates` (dirty settle, bulk-load rebuild, v121 backfill) | explicit thread bump, then `subagent_aggregates` upsert; its trigger stamps the anchor and completion siblings | rev |
| `EnsureProposedPlanState(WithParent)`, `MarkProposedPlanImplemented`, `CreateProposedPlanComment`, `UpdateProposedPlanComment`, `DeleteOrResolveProposedPlanComment`, `MarkProposedPlanCommentsSent` | explicit, on the thread id the mutator already carries | rev on the PLAN's thread |
| `RestoreFrom`'s row copy | triggers DROPped for the copy, recreated after | none during the copy: the restored counters are the snapshot's, verbatim |

**Window-visible DECORATION sources bump too.** `decorateSubagentAnchors`
is covered transitively because its inputs are item rows;
`decorateProposedPlanItems` is not. It rewrites `Item.Meta` from
`proposed_plans` and `proposed_plan_comments` on every window read
(`SyncThreadWindow` included), and neither table touches an `items` row,
so no trigger sees them. Their mutators call `bumpHistoryRevTx` on the
thread id they already carry. That is always the *plan's* thread, since
both tables reference `threads(id)` directly and
`MarkProposedPlanImplemented` is invoked cross-thread (the plan's thread
in, the implementing thread as data). Idempotent replays that write
nothing bump nothing. **Any future read-time projection of a non-`items`
table into a windowed row joins this class**: if a window read can render
it, its writers bump. A projection whose writers do not is the one bug
shape the contract cannot self-correct. The client is told its stale
copy is fresh, and nothing later contradicts it.

Rows invisible to windowed reads (subagent children and `plan_update`
notifications, which `paging.go` filters) still bump rev. That yields
false "stale" answers whose cost is one window fetch, i.e. exactly
today's behavior. Accepted; the contract trades precision for being
impossible to under-report.

### 3.3 Identity: `backend_id` and `replica_generation`

v55 also creates a one-row `store_meta` table:

- **`backend_id`** is a stable UUID minted once per store. It keys the
  client-side replica database (remote-access §10 already requires a
  backend UUID for deep links and multi-backend keying; this is that
  ID's birth).
- **`replica_generation`** is a UUID re-minted whenever rev/epoch
  continuity breaks for reasons the counters cannot express: today
  that is `RestoreFrom` (harness DB replace rewinds counters to
  snapshot values, so a client could hold stamps from a divergent
  future). Any future "replace the DB file" path must re-mint.

`store_meta` is an ordinary user table, so `RestoreFrom`'s row copy
replaces its row with the *snapshot's*, and a harness recording was
minted by a different store. The restore therefore reads the live
`backend_id` **before** the copy and writes it back alongside the fresh
generation; `remintStoreIdentityTx` takes it as a required parameter and
sets both columns, so "adopt the snapshot's identity" is not a state the
API can reach. A test that restores a self-snapshot cannot see this.
The fixture must use a snapshot from a genuinely different store.

Both ride the transport manifest the client already refetches on every
connect, plus every `SyncThreadWindow` response. The response copy
is the one that matters for a restore that happens mid-session, since
no reconnect refetches the manifest for a client that never
disconnected. A generation change, from either source, wipes EVERY tier
that holds stamp-paired rows: the replica database and the in-memory
L1 snapshot cache (its snapshots pair a
copied stamp with rows, so L1 is not exempt just because it is in
memory). The publisher side has the mirror obligation: whatever
replaces the database (today `RestoreFrom`) returns the new identity so
the caller MUST re-publish it to the manifest/sync path. A re-mint
nothing serves invalidates nothing. Never migrated, always dropped, the
same posture as the store's version-stamped render metadata (core
principle 3). A page-less answer that arrives with a changed generation
is also refused and re-asked stampless: a `fresh` across lineages is a
coincidental counter match, not freshness.

**"Changed" is per-asker, not per-process.** Observing the response
generation is a global side effect (it is what wipes the three tiers),
but *whether this answer crossed a lineage boundary* is a question about
the asker: the lineage it believed when it sent its stamp, versus the
lineage the answer names. With two panes in flight across one flip only
the first answer to land moves the global identity, so a "did the global
move?" test tells the second pane nothing changed about exactly the flip
that killed its painted rows. Each in-flight sync therefore captures the
generation it believes BEFORE its ask and compares the response against
that, while still routing the observation through the global wipe. Same
rule one level down: a pane that persists a window pins its attestation
to the generation it was minted under and declines to write once they
differ. A pane that was idle at the flip never observes anything, and
its dead-lineage rows must not reach the re-minted replica.

### 3.4 Client stamp discipline: understate, never overstate

An understated rev costs one redundant window fetch; an overstated one
would show stale content as "fresh" with no correction path. Every
stamping rule below is derived from that asymmetry, and the rules are
graded by durability:

- **Persisted (replica) stamps come only from `SyncThreadWindow`
  responses**, and the persisted rows must descend from that response
  (§6.1's reconcile-replaces rule). A sync response is the one place
  stamps and content are attested together in a single transaction.
  One subtlety: a page-less `fresh` echo attests only as much as the
  stamp the client *sent* was worth. Echoing an event-carried stamp
  confirms the server's counter, not that this client received every
  frame up to it. So a `fresh` answer upgrades a stamp to persistable
  only when the sent stamp was itself attested (a replica envelope or a
  prior sync). Requests send only the attested stamp paired
  with the painted window; event-carried counters cannot validate that window.

  **A verified held window is the second attestation source.** A request
  that painted rows also describes them (`haveWindow`, §5), and a
  page-less `fresh` earned that way attests exactly as a stamp-validated
  one does: the server re-derived those `(id, rev)` pairs from the
  database in the same read transaction as the stamp it returned, so the
  rows on screen ARE that read and the returned stamp describes them.
  This is what a turn on the open thread leaves a pane: no usable stamp,
  but every row it holds still current. The client side is
  `stores/threadWindowDigest.ts`. A held row always carries the revision of
  its latest pushed write: an upsert or patch that changes nothing rendered
  is absorbed onto the held row with its `rev` (`adoptRevIfEqual` in
  `stores/threadItems.ts`), so a re-persist of an unchanged row does not
  cost the next open a page. What still understates is a row the client
  mutated locally (a streaming delta, a mid-stream `meta` action): it keeps
  the rev of its last stamped write, fails verification and earns a page.
  Activity run members a page did not ship are still part of the window
  it describes: each run stub carries their digest, and the client folds
  it in (`windowDigestContribution` in `stores/activityRunStubs.ts`), so
  a window of prose plus stubs verifies as the rows it stands for. See
  [Timeline window pages](timeline-window-pages.md).

  **Attestation is a property of a WINDOW, not of a thread id.** What
  may be persisted is decided by the attestation the *pane holding the
  rows* carries, never by a thread-keyed lookup: a registry entry can
  name a page this pane never received (its write-back never fired, the
  pane later repainted from an older replica envelope, and the sync that
  would have converged them threw), and pairing those rows with that
  stamp is precisely the permanent false `fresh` this section exists to
  prevent. The pane's attestation is set from three sources: a sync page
  installing, a page-less `fresh` confirming rows whose description the
  server verified or whose attested stamp it echoed, and the ENVELOPE's
  own stamp the moment a replica window is painted (so a failed sync
  leaves the pane holding what its rows actually descend from). It
  is cleared by anything that changes the window's provenance (thread
  install, pane clear, structural cut, a page-less answer over an
  unattested source) and pinned to its lineage (§3.3). Item mutations
  invalidate the window attestation. Replay and reveal cursors can carry
  older content even when their delivery occurs after the read. Windows
  with active smoothers are also ineligible for stamped caching.

  A window holding an **optimistic row** (a send the wire has not
  echoed) is not a window any rev ever had, so neither stamped tier
  will pair a stamp with it: the replica skips the write entirely (its
  envelope IS the pairing) and the L1 snapshot drops both the row and
  its stamp. Filtering the row while keeping the stamp is NOT the safe
  version of this: the optimistic marker can itself be wrong (an echo
  that arrived under a different id leaves a real row marked), and that
  direction pairs a stamp with a window missing real content. Dropping
  the stamp costs one window fetch and cannot lie either way. The
  marker is discharged only by the wire. The pane's own optimistic
  insert must never clear it, or every filter downstream is dead code.
- **Event-carried stamps (`turn_completed`, `user_message:reverted`)
  do not validate client windows.** They are not full attestations: a
  writer outside the emitting goroutine, concretely the async
  highlight-span worker, can commit a rev bump before the stamp read
  while its frame reaches the client afterward, or never (disconnect).
  The client does not maintain a second revision registry from these events.
  History validation uses the stamp paired with its actual rows.
- **When unsure (transport gap, replay gap on any stamped or
  content-bearing channel), the client keeps the older stamp or drops
  to unknown.** The drop must reach every cache: an L1 snapshot carries a
  copy paired with its rows, and
  an unattested copy can name a rev whose frames the gap ate. It would
  spring a false `fresh` on the next warm re-entry and stay wrong for
  the session. Attested copies survive the gap, and that asymmetry is
  load-bearing rather than an optimisation: an attested stamp describes
  rows a sync returned, so any mutation since has advanced the backend's
  rev past it and the same `fresh` is unreachable. Dropping the whole
  cache would also be correct, but would discard every warm paint the
  gap never endangered.

## 4. Wire stamps on existing events

Three small additions, all read in (or immediately after) the
mutation's own transaction:

- **`provider:turn_completed`** gains `historyRev`/`historyEpoch`,
  one `SELECT` at event build (cold path). These counters describe backend
  history; they do not attest the client's loaded window (§3.4).
- **`user_message:reverted`** gains the post-cut stamps, read inside
  the cut transaction. The client invalidates cached windows and
  patches the visible window exactly like the backend cut
  (`removeRevertedItems`).
**Deletion carries no wire event and does not get one.** There is no
`thread:deleted` channel. A deleted thread leaves the sidebar through
the local action that deleted it (`threads.svelte.ts removeThread`,
which drops the replica entry, the history stamp and every other
per-thread cache in one place), and a client that did NOT perform the
deletion learns about it from the `gone` answer its next
`SyncThreadWindow` gets (§5), which drops the same set. Adding a
deletion event purely to reach the replica would buy nothing: the entry
it would clear is unpaintable the moment the thread is opened anyway,
and the cost of the miss is one cold open.

Ordinary `provider:item_event` frames stay unstamped. The streaming
path gains nothing per the remote-access budget rule
(`docs/specs/remote-access.md` §14), and the understate rule (§3.4) makes that
safe: a thread that streamed after the last stamp simply re-fetches its
window once.

## 5. `SyncThreadWindow` RPC

Replaces `ListThreadSliceAround` on the cold-open path (the paging
RPCs, `ListRecentTurns`, `GetThreadLiveState`, `SwitchThread`,
`AutoResumeThread` are unchanged).

```go
type SyncThreadWindowRequest struct {
    AnchorItemID string // '' = tail, else saved scroll anchor
    ItemBudget   int    // SLICE_AROUND_ITEM_BUDGET (200)
    HaveEpoch    int64  // -1 = no replica
    HaveRev      int64  // -1 = no replica / stamp unknown
    HaveWindow   *HeldWindow // nil when the caller holds no rows
}

// HeldWindow describes the rows the caller already holds, so a caller
// without an attested stamp can still be answered `fresh`.
type HeldWindow struct {
    OldestItemID string
    NewestItemID string
    Count        int
    HasMoreOlder bool
    HasMoreNewer bool
    Digest       string // 16 lowercase hex chars, see below
}

type SyncThreadWindowResponse struct {
    Status     string      // "fresh" | "stale" | "rewritten" | "gone"
    Epoch, Rev int64
    Generation string
    Page       *PagedItems // nil when fresh or gone
}
```

Decision order, all inside the one read transaction that reads the
stamps:

1. the stamps match → `fresh`, no page.
2. else `HaveWindow` verifies → `fresh`, no page, stamp = current.
3. else page, graded by the stamp compare.

Step 2 exists because a turn on the open thread moves the thread's rev,
which makes the caller's attested stamp worthless on the next open even
though every row it holds is still current. Verification is a read, not a
counter: both edge ids must resolve to visible top-level rows
(`windowedTimelineFilter`: `parent_id = ''` and not a `plan_update`
notification), oldest must not sit after newest, the rows in
`[oldest, newest]` under that filter ordered by `(turn_index, item_index)`
must number exactly `Count` (capped at `MaxHeldWindowItems`, 2000) and
hash to `Digest`, no row in the range may carry `rev < 0` (imported
history, §3.1), and an `EXISTS` probe on either side must agree with
`HasMoreOlder` / `HasMoreNewer`. The digest query selects `(id, rev)`
only and never joins payloads.

The digest is FNV-1a 64-bit over `id + 0x1f + decimal rev + 0x1e` per
row in window order, rendered as 16 lowercase hex characters. Go and
TypeScript implementations are pinned to one vector fixture
(`internal/store/testdata/window_digest_vectors.json`, copied byte-for-byte
to `frontend/src/test/fixtures/windowDigestVectors.json`).

`fresh` means the same thing whichever step produced it: the caller's
rows ARE the read, so it may attest them with the returned stamp and
write them back to the replica.

- **fresh**: `HaveEpoch/HaveRev` match the `threads` row, or `HaveWindow`
  verifies. No page.
  ~100-byte response; on a phone link this is the entire cold-open
  item transfer. This response *is* the degenerate case of the
  remote-access spec's reduced-snapshot primitive (§9): the full
  projection ships only when stamps prove it necessary.
- **stale**: epoch matches, rev doesn't. Page around the anchor,
  same shape and budget as `ListThreadSliceAround`. The page
  **replaces** the painted replica rows as the live window (§6.1).
  Replica content never outlives the reconcile, so the epoch taxonomy
  is a paint-safety grade, not something the merge has to trust.
- **rewritten**: epoch differs. Page attached and applied the same
  way; the difference is advisory paint posture. Cached rows may
  reference deleted or moved history, so a client that has not yet
  revealed the replica paint should prefer waiting for the page over
  revealing (§6.1 step 3 makes this automatic on fast links).
- **gone**: no thread row. Client drops the replica entry and
  surfaces the same not-found handling the current load path has.

Stamps and page MUST be read in one read-pool transaction (WAL
snapshot isolation) so the stamps attest exactly the returned rows.
The handler is read-only and runs on the read pool. It never touches
the single writer, so it stays fast even mid-turn (the wave 1 lesson).
Item ids are selected first and hydrated through indexed local/imported
physical branches. Joining the compound `timeline_payloads` view here is
forbidden: SQLite materializes that view before applying the selected item
window, turning a bounded open into a database-wide payload scan.

## 6. IndexedDB replica

New module `frontend/src/lib/replica/` (no IndexedDB exists in the
app today; `appStorage` is localStorage on the pinned transport origin
from §6.0).

### 6.0 Prerequisite: a stable page origin

IndexedDB is origin-scoped, and today the embedded webview's origin
changes every launch because the transport binds an ephemeral port.
That is the same churn that already forced `ui_state` server-side and
resets localStorage (see `ensureClientID`'s comment in `main.go`). Without a
fix the desktop replica would be empty on every boot, which is the
exact case this design targets.

Fix: when the resolved listen port is 0 (the ordinary desktop/WSL default,
or an explicit `--listen 127.0.0.1:0`), the backend binds a **persisted
per-install port**. First boot binds ephemeral and records the port
it got (`transport-port.json` next to `client-id.json`, atomicfile);
later boots re-bind it. Any bind failure falls back to ephemeral for
that run and adopts the new port, so a permanently squatted port
churns once, not forever, and a collision run merely re-primes the
replica. An explicit `--listen host:port` still wins unchanged.

Port obscurity was never a security control (the page credential and the
Host/Origin checks are), so pinning it costs nothing. The page cookie's
name carries the port, so a stable port also means a stable cookie name. Side benefit: webview
localStorage (the pre-hydration cache in `appStorage`) stops resetting
every launch. Paired `--connect` windows use the same port-pin helper under
`<configDir>/frontend/`, keeping their own browser preferences and attached-host
replicas across launches. The legacy launch-token relay retains an ephemeral
port; temporary token attachment does not create a durable pairing.

- **Database** `ao-replica-<backendId>`, per-backend keying per
  remote-access §10/§12. One object store `threads` keyed by
  `threadId`, one `meta` record `{generation, schemaVersion}`.
- **Database lifecycle**: the per-database caps below bound one
  database, and nothing inside a database nobody opens ever runs, so a
  backend id that MOVES would strand its database on the origin
  permanently. `purgeReplicaDatabases(liveBackendIds, token)`
  (`replica/session.ts`) is the cross-database sweep that closes that,
  and the same call is the purge sign-out and device revocation use
  (remote-access §9): the argument is the set of backends that remain
  attached, so an empty set drops the open databases too. Boot schedules
  it after the session is ready, never in front of the cold-open read.
  It is sequenced against the session by the same token every other
  replica operation carries — an identity change mid-sweep cancels the
  rest, and a target that some attached backend has open detaches THAT
  backend's session first. One replica session per attached backend
  (phase 7b): the database was already named per backend, so what
  changed is only that `replica/session.ts` holds a map instead of one
  slot, and the token every operation carries is what names the session
  it belongs to. A client that cannot name a live backend does not sweep at all,
  because an empty live set is an instruction, not an unknown. Where
  `indexedDB.databases()` is missing (Firefox before 126) only the open
  database can be named, so older orphans wait for an engine that can
  list them.
- **Envelope** per thread:

  ```ts
  {
    v: 1,
    cipher: 'none',            // encryption-ready: when remote-device
    body: {                    // encryption lands, body becomes an
      epoch, rev, savedAt,     // opaque encrypted blob and `cipher`
      items,                   // names the scheme (§12: keyed alongside
      oldestCursor,            // the session credential)
      newestCursor,
      hasMoreOlder, hasMoreNewer,
      latestSettledTurn,       // paint-only; ListRecentTurns re-fetches
      subagentFolds,
    },
  }
  ```

  Payload bodies are never stored (principle 4); `payloadMeta` /
  `payloadPreviewSpans` ride the item rows as they do on the wire.
- **Bounds**: reuse `threadItemCache`'s per-snapshot caps (1000 items /
  2 MiB chars), measured by the SAME estimator on both tiers, so a
  window one tier accepts is a window the other accepts; replica-wide
  cap 50 threads and 32 MiB, LRU-swept by `savedAt`.
- **Accounting is stored, not remembered**: one `meta` record lists
  `{threadId, savedAt, chars}` per envelope, and every commit re-reads
  and merges it INSIDE its own transaction rather than writing the
  page's copy wholesale. IndexedDB is origin-scoped, not page-scoped:
  a second page (a `--connect` window, a remote browser tab) shares the
  database, and a wholesale write would unaccount its envelopes,
  leaving them invisible to eviction until the next boot, i.e. an
  unbounded store.
  Merging also means the caps are enforced over the union, so a removal
  can evict as well as a write. The page keeps a mirror of the record
  purely so the removal path can answer "do we hold this thread?"
  without a transaction (the hot caller is the inactive-thread drop
  below), and re-reads it lazily whenever a commit rejected without
  saying whether it landed.
- **Lifecycle hooks**: generation mismatch and `schemaVersion` bump
  clear both stores as part of pointing the session at a backend
  (`initReplica`), which is also what a mid-session identity change
  runs. There is no separate clear entry point to keep in sync with
  it. Single entries are dropped by the local deletion / revert paths
  and by the sync `gone` answer (§4). The remote-access revocation
  signal (§12: revocation clears client caches) lands with phase 3 and
  gets its own hook then. Schema/DTO drift never migrates a replica.
  It drops it.
- **Inactive-thread drop**: a streaming thread with no mounted pane has
  its envelope dropped on the item-event flush, because nobody owns its
  window. That fires at flush rate for every background and workflow
  thread, most of which the replica has never held, so the drop
  short-circuits on the mirror before it opens a transaction.
  Since `provider:item_event` became entity-filtered the drop only
  reaches threads this client watches, so a thread with neither a pane
  nor a live-tail registration keeps its envelope while it streams
  elsewhere. Nothing is needed for that: the entry's attested stamp
  still predates the writes, so the next open answers `stale` and gets
  a replacing page — the understate rule (§3.4) covering a wider case
  than it was written for.
- **Desktop threat model note**: in the embedded webview the replica
  sits in the same OS-user boundary as the SQLite store itself, so v1
  plaintext adds no exposure. Encryption-at-rest becomes load-bearing
  only for remote browser devices, which is why the envelope reserves
  the field rather than shipping crypto now.

### 6.1 Cold-open flow (install, verify, then render)

`installCacheOrFreshState` gains an L2:

1. **L1** `threadItemCache` hit → install synchronously as verification
   evidence, but hold the timeline offscreen until the item-load leg
   verifies it. The item-load leg is never skipped.
2. **L1 miss** → the IndexedDB read runs BEFORE the RPC is issued, not
   concurrently with it. That ordering is load-bearing, not a lost
   optimization: the request's `haveEpoch`/`haveRev` come FROM the
   envelope, and a `fresh` answer obliges the client to keep the rows
   the stamp matched, so a stamp may only be sent once the content it
   describes is in hand. Reading first makes "answered fresh over
   nothing" unrepresentable rather than a case to recover from; the
   read is local, watchdog-bounded, and resolves null immediately on a
   disabled replica. (A replica read superseded by a newer thread
   switch is still discarded via the pane's `gen` token.)

   The request carries `haveWindow` too, whenever either tier supplied
   usable rows (`heldWindowOf` over those rows and the window's has-more
   flags). It is independent evidence, not a fallback: the stamp asks
   whether the thread changed, the window asks whether these rows are
   still the read, and a thread that had a turn while open can only
   answer the second. A pane holding an un-echoed optimistic row
   describes nothing, for the same reason it persists nothing (§3.4).
3. Replica installation goes through the existing `applyInitialSlice`
   path. The cached rows stay in the pane store for the sync request but
   are not rendered while it is pending. After ~100 ms the empty surface
   shows a loading indicator. Immediately before revealing the verified
   window, `armInitialSliceWarmup` closes the measurement gate over the
   rows that will actually mount. A verified cache hit still transfers no
   item page.
4. Sync response applied: **the page replaces the staged replica rows
   as the live window** (via `reconcileItemWindow` so unchanged rows
   keep `===` references and don't re-render). Replica rows are
   paint-only, and none survive into the live window past the reconcile.
   A byte-limited page first expands through fresh bounded reads to cover
   the retained window, as described in [timeline restoration](frontend-scroll.md).
   When those reads change the page, the combined window is not attested
   by the initial sync stamp. Concurrent item mutations or active reveal
   cursors also prevent attestation (§3.4).
   Merging replica scrollback from an older attestation under a newer
   stamp is the one composition that could pin a stale row under a
   false `fresh`, and this rule makes it unrepresentable. `fresh`
   applies nothing. The match that produced it — echoed stamp or
   verified window — attests the replica rows, which become the live
   window as-is under the stamp the answer returned.

   A page-less answer that neither echoes an attested stamp nor
   answers a described window is unreachable by those pairing rules. It
   is reported through `reportFrontendDiagnostic` and the ask is
   repeated with no evidence at all, so the pane converges on a page
   instead of sitting on rows nothing confirmed.

   A retry keeps its existing window visible and re-arms the warm gate
   only when it mounts the first rows or replaces a different backend
   lineage (§3.3).
5. **Write-back**: `snapshotOutgoingPane` (the sole L1 writer) also
   persists the envelope, taking rows from the live window and the
   stamp from the attestation the PANE carries for those rows (§3.4:
   never a thread-keyed lookup, never an event-carried stamp, never
   across a lineage re-mint, and never at all while an un-echoed
   optimistic row is in the window). A settled sync response for the
   open thread schedules a debounced write-back so a crash doesn't lose
   the session's threads. Fire-and-forget but never silent: IDB failures
   surface through the frontend error log
   (`uitrace.ReportFrontendErrorBatch` path), and a persistently
   failing replica degrades to today's behavior.

The current "L1 hit skips the item fetch entirely" behavior is
**removed**: every open fires `SyncThreadWindow`. Today's skip is a
real staleness hole (another attached device can rewrite history while
a thread sits in the in-memory cache); a `fresh` answer costs ~100
bytes and closes it.

Pagination beyond the window, `loadUntilItem`, search jumps: server
RPCs, unchanged. After a `rewritten` response the replica holds only
the freshly returned window, so there is nothing stale to page into.

## 7. Failure modes

| Scenario | Outcome |
|---|---|
| Replica stale, additive-only changes (`stale`) | One window fetch reconciles the staged rows before they become visible |
| Replica references deleted/moved rows (`rewritten`) | The retained extent is refreshed and deleted rows are dropped before the timeline becomes visible |
| Client stamp lost (gap, missed events) | Understated rev ⇒ `stale` ⇒ one redundant fetch. Never a false `fresh` |
| Backend DB replaced (`RestoreFrom`, future restore paths) | Generation mismatch on manifest ⇒ replica cleared wholesale |
| Thread deleted while cached | The deleting client drops the entry on the spot; any other client drops it on the `gone` answer (§4: there is no deletion event) |
| Initial history read exceeds its bounded deadline | Backend returns `temporarily_unavailable`; staged cached rows become visible with an explicit verification error, while an empty pane shows an in-place Retry action that re-runs only the history window sync |
| IndexedDB unavailable/quota/corrupt | Logged loudly, replica disabled for the session, behavior = today's cold open |
| Commit rejected without saying whether it landed (watchdog fires on a transaction that then commits) | The page marks its accounting mirror in doubt and re-reads the stored record before the next use of it; the stored record is authoritative and was already consistent, so nothing is stranded |
| Two pages on ONE origin (a `--connect` window, a second browser tab) | Envelopes are per thread, so the last writer of a thread wins it; the accounting record is merged inside each commit's transaction, so neither page unaccounts the other's envelopes and the caps hold over the union |
| Two panes / two devices on one thread | Stamps are per-backend truth; each client converges independently through its own sync calls. Write-backs are last-write-wins per thread envelope, safe because every envelope is server-derived, never client-invented |

## 8. Phasing

1. **Backend contract**: v55 migration (columns, `store_meta`,
   triggers), payload-mutator `threadID` plumbing, stamps on
   `turn_completed` / `user_message:reverted`, `SyncThreadWindow` +
   manifest generation. Standalone value: the L1 skip-hole closes as
   soon as the frontend calls sync on cache hits, before any
   IndexedDB work lands.
2. **Client replica**: `frontend/src/lib/replica/`, the
   `installCacheOrFreshState` L2, write-back, eviction, clear hooks.
3. **Remote era (deferred with remote-access)**: envelope encryption
   keyed alongside the session credential, revocation wiring, and the
   phone reduced-snapshot projection reusing the same stamps.

## 9. Testing

- **Contract transition table (Go)**: one table-driven test per §3.2
  row asserting `(rev, epoch)` deltas across the *sequence* of calls
  (insert→update→reposition→delete, import→refresh, fork→remap), not
  just single states (state coverage is not transition coverage).
  Raw-SQL trigger tests pin the structural guarantee independently of
  any store function.
- **Same-tx attestation**: a sync read racing a concurrent writer must
  return stamps matching its page (WAL snapshot), never the newer rev
  with older rows.
- **Row revisions (Go)**: exact `history_rev`/`items.rev` arithmetic for
  insert, update, delete and child writes; every exported item writer
  advances the touched row's rev and returns it; payload and plan writers
  stamp their owning item row; bulk load stamps the rows it inserts,
  leaves their anchors alone and still ends with `history_rev` above
  every rev it wrote; every pushed item event is its
  page read at the revision its write produced, including settle
  patches, while a deliberately altered wire row carries none; after a
  subagent turn the last push of every top-level row builds a window
  that verifies `fresh` (triage). The refresh read returns exactly the
  rows a write stamped without a push, the launch a completion settles,
  and a written row only once a sibling write moved it (store).
- **Held-window verification (Go)**: a window built from a real page
  verifies, and each way of being wrong is refused separately — count
  mismatch, a missing edge, either has-more flag flipped, a digest
  mismatch, a window over 2000 rows, and a window containing imported
  (rev -1) history, whose local tail still verifies. Rows a page would not
  return (child rows, plan_update notifications) must not affect the
  answer. A child write under a launch refuses a window holding only the
  launch's resume carriers and completion siblings, and the stamping
  statement's plan is index probes only.
- **Understate safety (frontend)**: applying unstamped upserts then
  re-opening yields `stale` + converged window; never `fresh` over
  divergent content.
- **Replica lifecycle (frontend)**: paint-then-reconcile (replica read
  strictly before the ask per §6.1 step 2, with superseding thread
  switches discarding a late replica read); generation clear across
  all three tiers including a mid-session change revealed by a sync
  response's generation (the coincidental-`fresh` refusal, driven with
  TWO panes in flight across one flip so a per-process "did it change?"
  answer cannot pass); `rewritten` scrollback drop; eviction under the
  char/thread caps; IDB failure degrades cleanly; and the cross-database
  sweep — a moved backend id's database reaped at the next open, a
  database this app did not mint left alone, an empty live set dropping
  the OPEN database (the sign-out contract), and a purge cancelled rather
  than deleting what a newer identity just opened.
- **Held-window description (frontend)**: `windowDigest` runs the same
  shared vectors the Go test does; the filter drops subagent children and
  plan_update notifications from the description; a reopen after a turn
  sends the mutated revs with no stamp; a `fresh` over that window
  attests it and writes it back; a refused window takes its page; and a
  page-less answer with neither a validating stamp nor a sent window is
  still reported and refetched.
- **Attestation pairing (frontend)**: a replica paint whose sync then
  FAILS must write back under the envelope's own stamp; a window
  carrying an optimistic row must reach neither stamped tier; a
  transport gap must strip unattested L1 stamp copies and leave
  attested ones. Each of these is a pairing, so cover the SEQUENCE that
  separates the stamp from its rows. A single-state check passes on
  the broken version.
- **Shared-origin accounting (frontend)**: a second connection to the
  same database mutating the stored index between this page's commits:
  its entries survive both a write and a removal, and the caps are
  enforced over the union; plus the desync-then-heal transition, where
  a commit rejects after landing and the next mirror read re-syncs.
- **E2E (harness)**: cold open of an imported/reverted/forked thread
  paints without resurrecting cut rows; `RestoreFrom` between opens
  clears the replica.
