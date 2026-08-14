# Thread Replica Sync — rev/epoch invalidation + IndexedDB replica

Status: DESIGN (wave 2 of the cold-thread-loading work). Nothing here is
implemented yet.

## 1. Problem

A cold thread open today costs one `ListThreadSliceAround` round trip
before any timeline content exists to paint. Wave 1 made the wait
artifact-free (warm-gate re-arm, size-prior carry-forward), but the wait
itself remains, and it scales with the link: ~5–40 ms on loopback,
hundreds of ms on the remote paths the remote-access spec plans for
(`docs/specs/remote-access.md` §9, §14). The in-memory
`threadItemCache` (LRU of 5 threads) already proves the fix — a cache
hit paints synchronously — but it dies with the page and covers almost
nothing after a restart.

Wave 2 makes the cached paint durable and makes its freshness *checkable
for ~100 bytes*:

- a per-thread **`rev`/`epoch`** stamp pair on the backend that any item
  mutation provably advances (the invalidation contract, §3),
- one RPC — **`SyncThreadWindow`** — that either answers "nothing
  changed" with no items attached, or returns the viewport window (§5),
- an **IndexedDB replica** of recently viewed thread windows that paints
  before the RPC returns and reconciles after (§6).

Non-goals for this wave: offline pagination (loadOlder/loadNewer stay
server-only), replicating payload bodies (principle 4 — heavy payloads
stay lazy-loaded), replicating turn rows or live state (their RPCs keep
firing on every open), CRDTs, event sourcing, or any change to the
reveal queue. The replica is a paint accelerator and transfer saver; the
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
  Codex ghost flips (`FlipGhostBackgroundRowsOnStart`), crash recovery
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
the mutation, and enforced structurally — not by asking every present
and future store function to remember to call a bump helper.

## 3. The invalidation contract

Two `INTEGER NOT NULL DEFAULT 0` columns on `threads` (migration v55):

- **`history_rev`** — advances on *every* persisted mutation that can
  change what a windowed item read for that thread returns: item
  insert/update/delete and payload content/meta/span writes.
  `rev` equality means "byte-identical window reads"; it says nothing
  about *what* changed.
- **`history_epoch`** — advances on mutations that a client holding a
  cached ordered window cannot survive by re-fetching a range:
  item deletion and item repositioning. Epoch says "your cached
  ordering may show rows that no longer exist or sit elsewhere — do not
  paint scrollback you haven't re-fetched".

Every epoch bump also bumps rev, so `rev` match alone implies fully
fresh; `epoch` exists to grade *how* stale a mismatch is.

**Rev 0 is unreachable for a thread that predates the contract.** v55's
SQL ends with `UPDATE threads SET history_rev = 1;`. Without it, every
pre-existing thread would sit at `(0, 0)` holding full history — and
`(0, 0)` is *also* the JSON zero value of the sync request's stamp pair,
so a client that omits the fields (or a bug that drops them) asks "is rev
0 still current?" and gets a page-less `fresh` over 400 items it does not
have. After the lift, `(0, 0)` can only describe a thread with no item
writes since v55, whose window is empty and for which a page-less `fresh`
is the truthful answer. The column DEFAULT stays 0 for exactly that
reason: a brand-new thread genuinely is at the origin.

### 3.1 Enforcement: triggers on `items`, thread-scoped payload API

Item-side bumps are SQLite triggers (installed by v55, alongside the
existing `trg_items_gc_*` precedent), so no store function — present or
future — can write an item row without advancing the contract:

```sql
CREATE TRIGGER trg_items_rev_insert AFTER INSERT ON items BEGIN
  UPDATE threads SET history_rev = history_rev + 1
   WHERE id = NEW.thread_id AND history_bulk_load = 0;
END;

CREATE TRIGGER trg_items_rev_update AFTER UPDATE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch
      + (OLD.turn_index IS NOT NEW.turn_index OR
         OLD.item_index IS NOT NEW.item_index OR
         OLD.thread_id  IS NOT NEW.thread_id)
  WHERE id IN (OLD.thread_id, NEW.thread_id) AND history_bulk_load = 0;
END;

CREATE TRIGGER trg_items_rev_delete AFTER DELETE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch + 1
  WHERE id = OLD.thread_id AND history_bulk_load = 0;
END;
```

Cost: the bump is an extra dirty page in the same transaction; WAL
writes pages per commit, not per statement, so a 500-row retention
chunk delete or a 10 Hz streaming append batch pays one extra page
image per commit. This satisfies the remote-access budget rule that
streaming must not gain per-frame work (§14) — the bump rides commits
that already exist.

`ApplyImportBatch` is the bulk-load exception without being a contract
exception. Inside its uncommitted transaction it sets the private
`threads.history_bulk_load` flag; the trigger predicates therefore skip their
per-row updates. After inserting the batch, the writer adds exactly the number
of inserted item rows to the revision and clears the flag. Readers cannot
observe it, a rollback restores it automatically, and committed revision
values remain identical to the ordinary trigger path.

Payload-side bumps stay explicit in the payload mutators rather than adding a
second trigger path. Since migration v58 payload rows carry `thread_id`, the
same scope selects the row and advances its owner's revision in one
transaction. The payload mutators that run *outside* an item-row transaction —
`ReplacePayloadData`, `UpdatePayloadMeta`, `UpdatePayloadSpans`, and
any bare `AppendPayloadData` — change signature to take `threadID` and
bump `history_rev` inside their own transaction. The signature is the
enforcement: a future caller cannot reach payload mutation without
naming the thread. (Their callers — triage stream finalize,
`rebuildCommandOutputMeta`, `persistPayloadSpans` — all know the
thread already.) For the signature to *be* the enforcement there must be
no way around it, so the bare `InsertPayload` / `UpsertPayload` exports
are removed: a payload is window-visible only through the item that
references it, and the private item-coupled upsert additionally resets
derived chunks, snapshots, and span blobs. Production
payload creation goes through the item-coupled writers
(`InsertItemWithPayload`, `AppendItemWithPayload`, `UpsertItem`).
The item-coupled combos
(`UpsertItemWithPayloadAppend`, `AppendItemSummaryAndPayloadData`) are
covered by the item triggers and need no second bump; double bumps
would be harmless anyway — the contract is monotonic, never exact.

### 3.2 Operation → contract map

| Operation (store) | Trigger path | Contract effect |
|---|---|---|
| `AppendItem`, `UpsertItem` insert, `AppendCompletionItem`, import tail append (`ApplyImportBatch`) | items INSERT | rev |
| `UpsertItemAtTurnHead` insert (below existing indices) | items INSERT | rev — additive; a stale paint merely lacks the row until reconcile |
| Streaming summary appends/tails, `UpsertItem` update, `UpdateItemMeta(-Merge)`, `UpdateItemFields`, lifecycle flips, `RecoverCrashedTurns`, ghost flips | items UPDATE | rev |
| `BumpItemToTurnEnd` (reposition) | items UPDATE (index changed) | **epoch** |
| `DeleteThreadItem`, `DeleteConversationFromTurn`, `DeleteConversationFromItem` | items DELETE | **epoch** |
| `ReplacePayloadData`, `UpdatePayloadMeta`, `UpdatePayloadSpans` (async span backfill), bare `AppendPayloadData` | explicit, new `threadID` param | rev |
| Fork clones (`CloneThreadItems`, `CloneThreadHistoryBeforeItem`) | items INSERT on the *target* thread | rev on target; source untouched |
| Import rollback / `DeleteThread` / retention sweep | thread row deleted | tombstone — replica entry dropped by the deleting client directly, and by any other client on the `gone` answer (§5) |
| `RestoreFrom` (harness snapshot) | whole-DB replace | **generation** re-mint (§3.3) |
| `decorateSubagentAnchors` (read-time meta projection) | none — no write occurs | covered transitively: its inputs are descendant item rows, whose writes bump rev |
| `EnsureProposedPlanState(WithParent)`, `MarkProposedPlanImplemented`, `CreateProposedPlanComment`, `UpdateProposedPlanComment`, `DeleteOrResolveProposedPlanComment`, `MarkProposedPlanCommentsSent` | explicit, on the thread id the mutator already carries | rev on the PLAN's thread |
| `RestoreFrom`'s row copy | triggers DROPped for the copy, recreated after | none during the copy — the restored counters are the snapshot's, verbatim |

**Window-visible DECORATION sources bump too.** `decorateSubagentAnchors`
is covered transitively because its inputs are item rows;
`decorateProposedPlanItems` is not. It rewrites `Item.Meta` from
`proposed_plans` and `proposed_plan_comments` on every window read
(`SyncThreadWindow` included), and neither table touches an `items` row,
so no trigger sees them. Their mutators call `bumpHistoryRevTx` on the
thread id they already carry — always the *plan's* thread, since both
tables reference `threads(id)` directly and
`MarkProposedPlanImplemented` is invoked cross-thread (the plan's thread
in, the implementing thread as data). Idempotent replays that write
nothing bump nothing. **Any future read-time projection of a non-`items`
table into a windowed row joins this class**: if a window read can render
it, its writers bump. A projection whose writers do not is the one bug
shape the contract cannot self-correct — the client is told its stale
copy is fresh, and nothing later contradicts it.

Rows invisible to windowed reads (subagent children, `plan_update`
notifications — `paging.go` filters) still bump rev. That yields false
"stale" answers whose cost is one window fetch — i.e. exactly today's
behavior. Accepted; the contract trades precision for being impossible
to under-report.

### 3.3 Identity: `backend_id` and `replica_generation`

v55 also creates a one-row `store_meta` table:

- **`backend_id`** — stable UUID minted once per store. Keys the
  client-side replica database (remote-access §10 already requires a
  backend UUID for deep links and multi-backend keying; this is that
  ID's birth).
- **`replica_generation`** — UUID re-minted whenever rev/epoch
  continuity breaks for reasons the counters cannot express: today
  that is `RestoreFrom` (harness DB replace rewinds counters to
  snapshot values, so a client could hold stamps from a divergent
  future). Any future "replace the DB file" path must re-mint.

`store_meta` is an ordinary user table, so `RestoreFrom`'s row copy
replaces its row with the *snapshot's* — and a harness recording was
minted by a different store. The restore therefore reads the live
`backend_id` **before** the copy and writes it back alongside the fresh
generation; `remintStoreIdentityTx` takes it as a required parameter and
sets both columns, so "adopt the snapshot's identity" is not a state the
API can reach. A test that restores a self-snapshot cannot see this —
the fixture must use a snapshot from a genuinely different store.

Both ride the transport manifest the client already refetches on every
connect, plus every `SyncThreadWindow` response — and the response copy
is the one that matters for a restore that happens mid-session, since
no reconnect refetches the manifest for a client that never
disconnected. A generation change, from either source, wipes EVERY tier
that holds stamps or stamp-paired rows: the replica database, the stamp
registry, and the in-memory L1 snapshot cache (its snapshots pair a
copied stamp with rows — L1 is not exempt just because it is in
memory). The publisher side has the mirror obligation: whatever
replaces the database (today `RestoreFrom`) returns the new identity so
the caller MUST re-publish it to the manifest/sync path — a re-mint
nothing serves invalidates nothing. Never migrated, always dropped —
same posture as the store's version-stamped render metadata (core
principle 3). A page-less answer that arrives with a changed generation
is also refused and re-asked stampless: a `fresh` across lineages is a
coincidental counter match, not freshness.

**"Changed" is per-asker, not per-process.** Observing the response
generation is a global side effect — it is what wipes the three tiers —
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
differ — a pane that was idle at the flip never observes anything, and
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
  frame up to it — so a `fresh` answer upgrades a stamp to persistable
  only when the sent stamp was itself attested (a replica envelope or a
  prior sync). The client registry therefore tags every stamp with
  whether a sync attested it: `latest` (any source) drives requests, and
  the tag is what a `fresh` echo consults before it may upgrade.

  **Attestation is a property of a WINDOW, not of a thread id.** What
  may be persisted is decided by the attestation the *pane holding the
  rows* carries, never by a thread-keyed lookup: a registry entry can
  name a page this pane never received (its write-back never fired, the
  pane later repainted from an older replica envelope, and the sync that
  would have converged them threw), and pairing those rows with that
  stamp is precisely the permanent false `fresh` this section exists to
  prevent. The pane's attestation is set when a sync page installs, when
  a page-less `fresh` confirms rows that came from an attested source,
  and — the case the registry cannot express — to the ENVELOPE's own
  stamp the moment a replica window is painted, so a failed sync leaves
  the pane holding what its rows actually descend from. It is cleared by
  anything that changes the window's provenance (thread install, pane
  clear, structural cut, a page-less answer over an unattested source)
  and pinned to its lineage (§3.3). Live upserts leave it alone: they
  arrive through the wire for this thread and only make the rows newer
  than the stamp, which is the safe direction.

  A window holding an **optimistic row** — a send the wire has not
  echoed — is not a window any rev ever had, so neither stamped tier
  will pair a stamp with it: the replica skips the write entirely (its
  envelope IS the pairing) and the L1 snapshot drops both the row and
  its stamp. Filtering the row while keeping the stamp is NOT the safe
  version of this: the optimistic marker can itself be wrong (an echo
  that arrived under a different id leaves a real row marked), and that
  direction pairs a stamp with a window missing real content. Dropping
  the stamp costs one window fetch and cannot lie either way. The
  marker is discharged only by the wire — the pane's own optimistic
  insert must never clear it, or every filter downstream is dead code.
- **Event-carried stamps (`turn_completed`, `user_message:reverted`)
  are adopted in memory only.** They are not full attestations: a
  writer outside the emitting goroutine — concretely the async
  highlight-span worker — can commit a rev bump before the stamp read
  while its frame reaches the client afterward, or never (disconnect).
  In memory that window is milliseconds and self-heals through frame
  delivery, replay, or the gap rule; persisted, it would be a
  permanent false `fresh` over content missing that write.
- **When unsure — transport gap, replay gap on any stamped or
  content-bearing channel — the client keeps the older stamp or drops
  to unknown.** The drop must reach every place a stamp LIVES, not just
  the registry: an L1 snapshot carries a copy paired with its rows, and
  an unattested copy can name a rev whose frames the gap ate — it would
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

- **`provider:turn_completed`** gains `historyRev`/`historyEpoch` —
  one `SELECT` at event build (cold path). Adopted in memory only
  (§3.4); within a session it lets a thread the user watched stream
  and then re-opened get a `fresh` answer instead of paying one
  convergence fetch.
- **`user_message:reverted`** gains the post-cut stamps, read inside
  the cut transaction. Same in-memory-only adoption; the client
  already patches the window exactly like the backend cut
  (`removeRevertedItems`).
**Deletion carries no wire event and does not get one.** There is no
`thread:deleted` channel — a deleted thread leaves the sidebar through
the local action that deleted it (`threads.svelte.ts removeThread`,
which drops the replica entry, the history stamp and every other
per-thread cache in one place), and a client that did NOT perform the
deletion learns about it from the `gone` answer its next
`SyncThreadWindow` gets (§5), which drops the same set. Adding a
deletion event purely to reach the replica would buy nothing: the entry
it would clear is unpaintable the moment the thread is opened anyway,
and the cost of the miss is one cold open.

Ordinary `provider:item_event` frames stay unstamped — the streaming
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
}

type SyncThreadWindowResponse struct {
    Status     string      // "fresh" | "stale" | "rewritten" | "gone"
    Epoch, Rev int64
    Generation string
    Page       *PagedItems // nil when fresh or gone
}
```

- **fresh** — `HaveEpoch/HaveRev` match the `threads` row. No page.
  ~100-byte response; on a phone link this is the entire cold-open
  item transfer. This response *is* the degenerate case of the
  remote-access spec's reduced-snapshot primitive (§9): the full
  projection ships only when stamps prove it necessary.
- **stale** — epoch matches, rev doesn't. Page around the anchor,
  same shape and budget as `ListThreadSliceAround`. The page
  **replaces** the painted replica rows as the live window (§6.1) —
  replica content never outlives the reconcile, so the epoch taxonomy
  is a paint-safety grade, not something the merge has to trust.
- **rewritten** — epoch differs. Page attached and applied the same
  way; the difference is advisory paint posture — cached rows may
  reference deleted or moved history, so a client that has not yet
  revealed the replica paint should prefer waiting for the page over
  revealing (§6.1 step 3 makes this automatic on fast links).
- **gone** — no thread row. Client drops the replica entry and
  surfaces the same not-found handling the current load path has.

Stamps and page MUST be read in one read-pool transaction (WAL
snapshot isolation) so the stamps attest exactly the returned rows.
The handler is read-only and runs on the read pool — it never touches
the single writer, so it stays fast even mid-turn (the wave 1 lesson).

## 6. IndexedDB replica

New module `frontend/src/lib/replica/` (no IndexedDB exists in the
app today; `appStorage` is server-backed `ui_state` and localStorage
does not survive the per-launch origin change).

### 6.0 Prerequisite: a stable page origin

IndexedDB is origin-scoped, and today the embedded webview's origin
changes every launch because the transport binds an ephemeral port —
the same churn that already forced `ui_state` server-side and resets
localStorage (see `ensureClientID`'s comment in `main.go`). Without a
fix the desktop replica would be empty on every boot, which is the
exact case this design targets.

Fix: when the resolved listen port is 0 (no `--listen`, or the WSL
launcher's `--listen 127.0.0.1:0`), the backend binds a **persisted
per-install port** — first boot binds ephemeral and records the port
it got (`transport-port.json` next to `client-id.json`, atomicfile);
later boots re-bind it. Any bind failure falls back to ephemeral for
that run and adopts the new port, so a permanently squatted port
churns once, not forever, and a collision run merely re-primes the
replica. An explicit `--listen host:port` still wins unchanged.

Port obscurity was never a security control — the bootstrap token and
Host checks are — so pinning it costs nothing. Side benefit: webview
localStorage (the pre-hydration cache in `appStorage`) stops resetting
every launch. The `--connect` client-mode stub keeps its ephemeral
port for now; it can adopt the same helper when remote-attach windows
want durable replicas.

- **Database** `ao-replica-<backendId>` — per-backend keying per
  remote-access §10/§12. One object store `threads` keyed by
  `threadId`, one `meta` record `{generation, schemaVersion}`.
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
  2 MiB chars) — measured by the SAME estimator on both tiers, so a
  window one tier accepts is a window the other accepts; replica-wide
  cap 50 threads and 32 MiB, LRU-swept by `savedAt`.
- **Accounting is stored, not remembered**: one `meta` record lists
  `{threadId, savedAt, chars}` per envelope, and every commit re-reads
  and merges it INSIDE its own transaction rather than writing the
  page's copy wholesale. IndexedDB is origin-scoped, not page-scoped:
  a second page (a `--connect` window, a remote browser tab) shares the
  database, and a wholesale write would unaccount its envelopes —
  invisible to eviction until the next boot, i.e. an unbounded store.
  Merging also means the caps are enforced over the union, so a removal
  can evict as well as a write. The page keeps a mirror of the record
  purely so the removal path can answer "do we hold this thread?"
  without a transaction — the hot caller is the inactive-thread drop
  below — and re-reads it lazily whenever a commit rejected without
  saying whether it landed.
- **Lifecycle hooks**: generation mismatch and `schemaVersion` bump
  clear both stores as part of pointing the session at a backend
  (`initReplica`), which is also what a mid-session identity change
  runs — there is no separate clear entry point to keep in sync with
  it. Single entries are dropped by the local deletion / revert paths
  and by the sync `gone` answer (§4). The remote-access revocation
  signal (§12: revocation clears client caches) lands with phase 3 and
  gets its own hook then. Schema/DTO drift never migrates a replica —
  it drops it.
- **Inactive-thread drop**: a streaming thread with no mounted pane has
  its envelope dropped on the item-event flush, because nobody owns its
  window. That fires at flush rate for every background and workflow
  thread, most of which the replica has never held, so the drop
  short-circuits on the mirror before it opens a transaction.
- **Desktop threat model note**: in the embedded webview the replica
  sits in the same OS-user boundary as the SQLite store itself, so v1
  plaintext adds no exposure. Encryption-at-rest becomes load-bearing
  only for remote browser devices, which is why the envelope reserves
  the field rather than shipping crypto now.

### 6.1 Cold-open flow (render-from-replica-then-reconcile)

`installCacheOrFreshState` gains an L2:

1. **L1** `threadItemCache` hit → paint synchronously (unchanged) —
   but the item-load leg is no longer skipped (see below).
2. **L1 miss** → the IndexedDB read runs BEFORE the RPC is issued, not
   concurrently with it. That ordering is load-bearing, not a lost
   optimization: the request's `haveEpoch`/`haveRev` come FROM the
   envelope, and a `fresh` answer obliges the client to keep the rows
   the stamp matched — so a stamp may only be sent once the content it
   describes is in hand. Reading first makes "answered fresh over
   nothing" unrepresentable rather than a case to recover from; the
   read is local, watchdog-bounded, and resolves null immediately on a
   disabled replica. (A replica read superseded by a newer thread
   switch is still discarded via the pane's `gen` token.)
3. Replica paint goes through the existing `replaceTimelineItems`
   chokepoint and **arms the wave 1 warm gate exactly like an initial
   slice** (`armInitialSliceWarmup`). On loopback the sync response
   usually lands inside the ~100 ms quiet window, so reconciliation
   happens *before first reveal* — zero artifacts. On slow links the
   replica reveals first and the reconcile patches visible content;
   that is the explicit remote tradeoff and it is strictly better than
   the blank pane it replaces.
4. Sync response applied: **the page replaces the painted replica rows
   as the live window** (via `reconcileItemWindow` so unchanged rows
   keep `===` references and don't re-render). Replica rows are
   paint-only — none survive into the live window past the reconcile.
   This is what makes write-back safe: the persisted rows always
   descend from the attested page (plus later live events, which only
   make content *newer* than the stamp — the safe direction, §3.4).
   Merging replica scrollback from an older attestation under a newer
   stamp is the one composition that could pin a stale row under a
   false `fresh`, and this rule makes it unrepresentable. `fresh`
   applies nothing — the fresh match itself attests the replica rows,
   which become the live window as-is.

   The warm gate is NOT re-armed for this page: it lands over a window
   the reader may already be looking at, and re-closing the gate there
   blanks content that is on screen. The one exception is a lineage
   change (§3.3): those painted rows belong to a history the backend no
   longer has, so the page does not reconcile them, it replaces all of
   them — a first content mount in everything but name, and it re-arms
   like one.
5. **Write-back**: `snapshotOutgoingPane` (the sole L1 writer) also
   persists the envelope — rows from the live window, stamp from the
   attestation the PANE carries for those rows (§3.4: never a
   thread-keyed lookup, never an event-carried stamp, never across a
   lineage re-mint, and never at all while an un-echoed optimistic row
   is in the window) — and a settled sync response for the open thread
   schedules a debounced write-back so a crash doesn't lose the
   session's threads. Fire-and-forget but never silent: IDB failures
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
| Replica stale, additive-only changes (`stale`) | Instant paint, one window fetch, in-range reconcile; missing tail rows appear on reconcile |
| Replica references deleted/moved rows (`rewritten`) | Paint may briefly show removed content on slow links (hidden by the warm gate on fast ones); hard-replaced on response; scrollback outside window dropped |
| Client stamp lost (gap, missed events) | Understated rev ⇒ `stale` ⇒ one redundant fetch. Never a false `fresh` |
| Backend DB replaced (`RestoreFrom`, future restore paths) | Generation mismatch on manifest ⇒ replica cleared wholesale |
| Thread deleted while cached | The deleting client drops the entry on the spot; any other client drops it on the `gone` answer (§4 — there is no deletion event) |
| IndexedDB unavailable/quota/corrupt | Logged loudly, replica disabled for the session, behavior = today's cold open |
| Commit rejected without saying whether it landed (watchdog fires on a transaction that then commits) | The page marks its accounting mirror in doubt and re-reads the stored record before the next use of it; the stored record is authoritative and was already consistent, so nothing is stranded |
| Two pages on ONE origin (a `--connect` window, a second browser tab) | Envelopes are per thread, so the last writer of a thread wins it; the accounting record is merged inside each commit's transaction, so neither page unaccounts the other's envelopes and the caps hold over the union |
| Two panes / two devices on one thread | Stamps are per-backend truth; each client converges independently through its own sync calls. Write-backs are last-write-wins per thread envelope — safe because every envelope is server-derived, never client-invented |

## 8. Phasing

1. **Backend contract** — v55 migration (columns, `store_meta`,
   triggers), payload-mutator `threadID` plumbing, stamps on
   `turn_completed` / `user_message:reverted`, `SyncThreadWindow` +
   manifest generation. Standalone value: the L1 skip-hole closes as
   soon as the frontend calls sync on cache hits, before any
   IndexedDB work lands.
2. **Client replica** — `frontend/src/lib/replica/`, the
   `installCacheOrFreshState` L2, write-back, eviction, clear hooks.
3. **Remote era (deferred with remote-access)** — envelope encryption
   keyed alongside the session credential, revocation wiring, and the
   phone reduced-snapshot projection reusing the same stamps.

## 9. Testing

- **Contract transition table (Go)**: one table-driven test per §3.2
  row asserting `(rev, epoch)` deltas across the *sequence* of calls —
  insert→update→reposition→delete, import→refresh, fork→remap — not
  just single states (state coverage is not transition coverage).
  Raw-SQL trigger tests pin the structural guarantee independently of
  any store function.
- **Same-tx attestation**: a sync read racing a concurrent writer must
  return stamps matching its page (WAL snapshot), never the newer rev
  with older rows.
- **Understate safety (frontend)**: applying unstamped upserts then
  re-opening yields `stale` + converged window; never `fresh` over
  divergent content.
- **Replica lifecycle (frontend)**: paint-then-reconcile (replica read
  strictly before the ask — see §6.1 step 2 — with superseding thread
  switches discarding a late replica read); generation clear across
  all three tiers including a mid-session change revealed by a sync
  response's generation (the coincidental-`fresh` refusal, driven with
  TWO panes in flight across one flip so a per-process "did it change?"
  answer cannot pass); `rewritten` scrollback drop; eviction under the
  char/thread caps; IDB failure degrades cleanly.
- **Attestation pairing (frontend)**: a replica paint whose sync then
  FAILS must write back under the envelope's stamp even when the
  registry holds a newer attested one for that thread; a window
  carrying an optimistic row must reach neither stamped tier; a
  transport gap must strip unattested L1 stamp copies and leave
  attested ones. Each of these is a pairing, so cover the SEQUENCE that
  separates the stamp from its rows — a single-state check passes on
  the broken version.
- **Shared-origin accounting (frontend)**: a second connection to the
  same database mutating the stored index between this page's commits —
  its entries survive both a write and a removal, and the caps are
  enforced over the union; plus the desync-then-heal transition, where
  a commit rejects after landing and the next mirror read re-syncs.
- **E2E (harness)**: cold open of an imported/reverted/forked thread
  paints without resurrecting cut rows; `RestoreFrom` between opens
  clears the replica.
