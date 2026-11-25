# Frontend stores

This directory owns shared reactive application state, wire-event reactions,
and entity-owned RPC lifecycles. Components consume store state through
`$derived`; they do not subscribe to transport channels or issue entity-owned
RPCs directly. `src/lib/architecture.test.ts` enforces these boundaries. Remove
an allowlist entry when its exception is fixed.

## Choosing a state primitive

- Use `entityStore.svelte.ts` for a resource that must be acquired, released,
  suspended, and reacquired. `backendForKey` is required: a backend key means
  computer-owned, `null` means frontend-owned, and `undefined` means ownership
  is not known yet. Unknown ownership must reach transport routing; never replace
  it with HOME. Only the owning backend's connection may suspend or restart an
  entry. Route every update through `apply` so reconciliation cannot be bypassed.
- Use `keyedSignalRegistry.svelte.ts` for push-fed keyed state with no resource
  lifecycle. `set` is the only box creator. Svelte does not track state created
  inside the reaction currently reading it.
- Use `snapshotStream.ts` whenever a call's answer is state the frames of a
  channel also change. The transport writes events queued for a connection
  ahead of a reply, including ones published after the method returned, so
  frames of work a call started can precede its answer, and can name a key
  only that answer reveals. `hydrate` reads a key's snapshot
  (newest-read-wins); `claim` runs any other call, holding one key, or
  every key when the answer names one the client does not know yet. Both
  buffer the held keys' frames and replay them over the applied answer; the
  owning store keeps the state, what an answer means, and how a frame folds
  in. When the producer publishes outside the lock that orders its states,
  it numbers them (a per-process `Seq`) and the fold keeps the newest per
  launch through `createSequenceGate`. Never apply an answer over frames
  without one of these.
- Entity values use deep `$state` by default. Set `rawValue: true` only when all
  writers replace values wholesale. In-place mutation of a raw value does not
  wake readers.

Allocate reactive state at attachment or module setup boundaries, not lazily
inside a getter read by `$derived`. Read fallback snapshots with `untrack` when a
loader will replace the state it reads, or the load can become an asynchronous
refresh loop.

## Ownership, routing, and permissions

Capture an entity's backend before an asynchronous operation and use it for every
follow-up. Paths, focused panes, HOME, and the selected computer do not identify
an existing entity's owner. Drafts route through their project until a thread
exists. Use `transport/entityScopes.ts` for entity scope checks and
`selectedBackend.svelte.ts` only for methods generated with route `selected`.

Scope grants are transport facts, not store state. Import `hasScope` directly
from `transport/scopes.ts`; do not wrap or re-export it. A passive loader checks
the exact RPC scope before firing, except a read whose answer is what says
a computer lacks the feature or refuses this device (the tracker
connections): it asks whatever the grant, and that answer stands until the
computer connects again (`trackerConnections.svelte.ts`). Mount effects may rely on scope reactivity,
but one-shot startup work must await `pageGrantsResolved()`. A thread switch can
run before the manifest from the replica catalog and must not wait for it: its
gated legs check `pageGrantsKnown()` and run from `pageGrantsResolved()`.
Suppress only typed passive connection failures with
`isPassiveConnectionFailure`; writes, explicit actions, scope errors, and file
errors remain visible.

One thread may be mounted in only one pane. Enter through `mountThreadInPane`;
`replaceThreadInPane` stays private. All writes to a pane's thread identity go
through `assignThread`, which refreshes transport watches and saves the pane
layout before any load the change starts. Use `revealPane` for actions that
expose a pane so compact layout moves to the thread screen.

Tool panes are layout items only (`toolPanes.svelte.ts`), one per kind.
A ticket pane is a layout item and a record (`ticketPanes.svelte.ts`),
which owns the ticket it shows and is dropped with its item. A
thread open that names no pane resolves through
`panes.svelte#focusedOpenTargetPane`, which gives a focused tool or ticket
pane's slot to the thread; do not pass `getFocusedPaneOrNull() ?? ensureMainPane()` as a
target, which bypasses it. A tool's open that answers after an await goes
through `mountThreadForToolPane` with the tool pane, or null once the
surface that asked is gone, so it never lands in a pane that gained focus
meanwhile.

## Events and convergence

`events.ts` is the single subscription root. Add a channel there and put its
reaction in the matching `events*.ts` module. Every event includes its origin as
the second `wailsEventOn` argument; an empty `backendId` means unknown origin.

Treat persisted row events as distributed convergence:

- `thread:updated` and `project:updated` apply the complete row for `full` and
  `listed`; `unlisted` and `deleted` remove it. A thread `emptied` frame removes
  only its sidebar row: panes showing it and its per-thread state stay. A thread
  `full` frame never adds unknown sidebar membership. A `patch` changes only its
  named fields.
- A thread row an RPC returns is applied with `syncThreadReply` (or
  `threadReplyRow`) and a `threadRowFrameMark` taken before the call, so a
  frame that arrived during the call is not undone by the older reply.
- Explicit unread is `lastReadAt === 0`, so numeric newest-wins is insufficient.
  Keep read and unread RPCs behind `threadReadWrites.ts`; its local claim spans
  both the RPC and local patch.
- Client-specific failures compare `connectionId`, not `deviceId`. Different
  tabs share a device but own independent attempts. Accept unstamped
  compatibility frames.
- `draft:updated` never carries draft text. Re-read only for a mounted pane, and
  do not overwrite this connection's echo or a locally pending snapshot.
- Invalidation-only settings and catalog events re-read authoritative state.
  Serialize the read with local writes and discard replies superseded by a newer
  local mutation or pushed frame.

Gap recovery belongs to `transportRecovery.ts`. Register snapshot reads with
`holdBackendRecovery`; completion is per backend and waits for queued item
mutations that existed when replay ended. A gap snapshot must not overtake older
queued replay, and later live events must not extend that recovery wait.

A PR's `pr:updated` and `pr:ci_updated` frames carry its pump's one
sequence, so both rank against one watermark per PR key, seeded by the
`SubscribePRUpdates` result, and a join replays buffered frames in sequence
order. A PR thread read (`GetPRDetail` with `ListPRReviewThreads`)
carries no sequence: take `prReadStart` before the call and pass it to
`applyPRSnapshot` or `applyPRThreads`, which keep what a frame brought
during the read. A merge action answers with the pump's sequence after
the poll it asked for; the pane reads as pending until the watermark reaches it, and
gives the wait up when the backend's launch (its hello's `launchId`) changes, since
a restarted backend's pump numbers from one again, while a re-subscribe to
the same process keeps it. A PR's forge facts (`forgeFacts.svelte.ts`)
are asked for from its applied snapshots, once per changed body (or moved
head, for a body whose keys embed it), and drop with its entry. The
frame's revision is the reader's own answer (`REVISION_FACT_KEY`), never
asked for, so a moved head lexes again only the blocks that read it. A key
a reader is asked for that no snapshot body named (a draft, an editor
preview, a form the scan missed) is asked for once, by the next
microtask. Nothing asks per mount or timer, and nothing on a computer
whose binding answered null until its connection changes. A found answer
is never asked for again by the clock; only an answer that can change on
its own is (unavailable, or an LFS object not local, a minute after it;
absent ten minutes after it), and the reader's refresh or reload asks the
forge again for everything the PR holds (`reloadForgeFacts`, `fresh`). A
GitLab body's labels are one fact, the project's label list, whose answer
object the PRs of a project share. A found list counts as absent while a
lex under it decided a label a body names is not in it (the tokenizer
calls `factDenied`, the store marks that answer until it changes or no
body names such a label). CI is sourced only by
that subscription, or for a repository pipeline by its `SubscribePipeline`
handle (`pipelineCI.svelte.ts`), whose `ci:pipeline_updated` frames rank
the same way against one watermark per pipeline key, seeded by the result,
with the frames of its join replayed in sequence order; the frontend never
polls it. Both feed the CI entry under one CI key and register their follow,
refresh and action calls and their applied CI sequence in `ciHandles.ts`.
A CI action (`ciActions.svelte.ts`) is held under the run identities the
backend reserves for it and reads as pending on every CI key whose
pipeline shows one; it is pending until the initiating key's sequence
reaches the one the action answered with, as a merge action is.
`pr:ci_log` frames carry a followed job's whole log state, its index and
never its text, so the newest by sequence wins; the follow set lives on the
subscription handle, so it is restated after every subscribe and after a
gap on `pr:ci_log`. The log view reads lines through
`ciLogBlocks.svelte.ts`, a bounded block cache that keeps a block's lines
across a version only within the index's `stableLines`.

A workspace's git status has four writers (the `GitStatusSubscribe` reply,
`git:status` frames, `refreshGitStatus` and the checkout answer of
`withCheckoutAnswer`), and every status carries the `seq` its backend
process took before its reads began. `gitStatusStore.svelte.ts#applyStatus`
is the only way a status reaches the store, and orders it by the seq of the
value the key holds: below it nothing is written, equal to it the held value
stays (`keepValue`: an error clears, no reader wakes), above it the status
applies. The entity store drops the value on every transport edge of the
key's backend, which is what lets a restarted backend's count begin again;
a re-source on the same connection keeps the value and orders against it.
A frame that overtakes its subscribe reply is kept, newest per
backend and cwd, while a subscribe is in flight on that backend, and the
reply applies it. An unsequenced status (an older backend) is never kept and
applies as it arrives, except a checkout answer, which stands down when any
status landed during the call.

Live turn, approval, user-input, compacting and sends-pending state is
backend state, never derived from items or rows. Sends pending
(`provider:sends_pending`, capability `sends-pending.v1`) covers a queued or
flushed message until its echo while the thread has a session live or
starting, so every thread surface, the pane's own composer included, reads
running from it rather than from the queue frames. `RegisterQueueItem`
replies with the thread's answer and the channel sequence it reflects. The
reply can overtake earlier frames, so a frame at or below an applied answer
is dropped, and an answer that the computer's stream (frames and gap
markers, per `launchId`) has already passed is not applied.
`threadLiveActivity.ts` reads `ListThreadLiveActivity` for every thread of a
computer on each connection edge and after a gap on one of those channels,
one read per computer at a time; a pane additionally reads
`GetThreadLiveState` when it mounts. A push that lands while a snapshot is
in flight wins over the snapshot.

A flushed message of a thread with no pane leaves the send-queue mirror when
the backend settles it (`flushSettlement.ts`, capability
`flush-settlement.v1`): by the `flushedThrough` of a sends-pending frame, or
by `ListUnsettledFlushes`, read on every connection edge and after a gap on
`provider:sends_pending`. Both are facts about a `provider:queue_flushed`
position, so they apply in any order. The contract is in
[turn-lifecycle.md](../../../../docs/architecture/turn-lifecycle.md).

`watchedThreads.ts` unions registered sources for every thread whose surface
exists and for every subagent scope a surface reads. Watches never depend on
focus, visibility, or `document.hidden`. Registering a consumer of an entity-filtered channel also requires contributing
its thread IDs. A watched thread delivers only root rows on
`provider:item_event`: a consumer of rows with `parentId` set contributes
their scope while it exists, or reads anchor-level data instead. Push the
opening watch set before history loads, including a scoped surface's scopes
before its first read, and restate it whenever a pane adopts or clears a
thread or a surface's scopes change.

`screenPresence.ts` reports focus and visible panes only so the backend can
suppress redundant OS notifications, and `currentScreenPresence` answers the
same two facts to `browserNotificationPresenter.svelte.ts`, which runs that
decision locally for a screen the host cannot judge. Do not use presence to
govern subscriptions, delivery, fetching, rendering, or other work.

Every screen presents its own notifications. A page that is not loopback-served
and not the native shell runs `notifications/gate.ts` against its own settings
and its own focus and raises a Web Notification; the host presents for the
backend machine's own screen, and `notification:sound` stays loopback-only
because the decision behind it is that screen's. The `replayed` argument
`wailsEventOn` passes marks a frame drained out of the reconnect replay
window: only a subscriber that interrupts a person may read it.

Transport replay, watch splitting, and gap rules are documented in
[transport.md](../../../../docs/architecture/transport.md#event-replay-and-filtering).
Offline catalog ownership and replica invalidation are documented in
[thread-replica-sync.md](../../../../docs/architecture/thread-replica-sync.md).

## Computer-owned and frontend-owned state

Keep per-computer catalogs, settings, workflow state, editor preferences, and
service state keyed by backend. A disconnect retains the last useful snapshot;
a detach removes that computer's state. Late replies must prove their backend,
request generation, and relevant catalog or mutation revision before applying.
Ownership moves invalidate thread history stamps, item caches, interrupt state,
pending reads, and watched-thread routing for the old owner.

`catalogLoad.svelte.ts` holds whether each computer's thread, project and
group catalogs have loaded, and owns their retries: one timer per registered
reader and computer, armed while any catalog that reader covers wants one and
no attempt is in flight. Every catalog read, whatever started it, is an
attempt (`beginCatalogAttempt`) from its begin until `computerCatalogReads.ts`
releases it, when it ends or a newer read supersedes it: beginning one
consumes the retry and cancels the armed timer, so a retry never supersedes
a live read, and Retry then only restarts the schedule. A read past its
startup deadline whose catalog has not loaded is the one exception: its
attempt ends there and one replacement, which waits for its answer, follows
on the retry schedule; a loaded catalog waits for the late outcome. A catalog
that has not loaded is never presented as empty; a loaded one that fails
shows its error beside the rows it keeps. Every read settles there: a
failure inside `commitShares` (`computerRows.ts`), an answer in the `commit`
callback that commits its rows. Answers commit in the same synchronous block
that replays local mutations; never commit an answer after an `await`.

A computer's projects, the threads under them and the groups that gather
those threads are one tree in the sidebar, read only as a set by
`loadSidebarCatalogs` (`sidebarCatalogs.ts`), which owns their supersession,
commit timing, the sidebar listing (`catalogTreeListed`) and the rows the
boot's layout restore validates against. Nothing else reads any of the three:
a second reader would supersede the round's and leave its tree held. A listed
computer's three answers commit together once no read is out, so its rows
change arrangement at most once per round; an unlisted computer's commit at
their own windows, for the panes and pickers that read those stores, while
`catalogTreeListed` holds every row of its tree. The set is atomic in the
sidebar without exception: when one read fails, on any attempt, the others'
answers are never listed. While a catalog keeps failing, the sidebar shows
its error row with Retry beside the last complete tree, or alone if there was
none, and never a half-arranged list, so a computer whose read keeps failing
before it ever listed shows no project header. A computer lists the first
time one read of it is a complete tree: all three answered in that round, or
rows standing in that are one kept tree by their provenance, which
`catalogLoad.svelte.ts` owns. The replica keeps only complete trees and the
changes pushed onto them, and each computer's write queue
(`computerCatalogWriter.ts`) is the one writer of its three records; the
architecture test refuses any other caller. The module comments have the
rules.

Only a newer read of the same computer supersedes a sidebar catalog read.
Change thread, project and group rows through the catalog writer's `mutate`,
a pure transform that `computerCatalogReads.ts` records for the reads of that
computer in flight and replays over their answers. A pushed or local change
must never drop an answer: nothing would re-read a catalog that has not
loaded. The transform carries the change's whole meaning over any answer: a
`listed` frame upserts its row (`listThread`, `listProjectLocal`), since the
answer it replays over may lack a row the client already held.

Frontend preferences and appearance libraries remain local to the frontend and
survive host removal. Mirror only generated `FRONTEND_DEVICE_SETTINGS_KEYS` to
computer device buckets, without holding a local save open for an unavailable
computer. Appearance file locality is decided only after grants resolve.

Thread and project catalogs from several computers merge by repository identity,
never by filesystem path. The merge is over every row the client holds, so an
entry's identity and representative do not depend on what a surface shows; a
surface showing some members aggregates those itself. Which listed rows form a
sidebar entry, and which earlier row it continues, is decided in
`sidebarEntryContinuity.svelte.ts`; what the sidebar's rows are is one value
from `components/sidebar/sidebarEntries.ts`, the only module in that directory
that may import from this store or that one beyond the writers
`lib/architecture.test.ts` lists. The manual reorder is the
store's (`reorderProjects`): the order the user dragged is an overlay the
sidebar sorts by (`projectOrderPosition`), never a write to the rows, so the
rows carry the last order a computer accepted and a refusal drops the overlay
with nothing to undo. Nothing folds an accepted order into the rows: the
computer broadcasts one frame per row it moved before it returns, and the
transport delivers an event queued before a reply ahead of that reply
(`internal/transport/conn.go#writeReply`), so the rows carry the order by the
time the overlay is dropped. One write at a time, and only the newest order
waiting behind it is sent. A group's and a section's fold and the Assistants
section's and a group's list size live in `sidebar.svelte.ts`, which also
holds the record factories (`keySetRecord`, `listLimitRecord`); only
`sidebarEntryContinuity.svelte.ts` may import them, which `lib/architecture.test.ts`
enforces through re-exports. A project row's fold and list size live in records
of their own, keyed by the entry, which `sidebarEntryContinuity.svelte.ts`
creates and exports nothing to read or write by key. When an entry's key moves
under a row that stays, it copies the row's fold and limit to the new key and
clears the old one, and its writers commit a waiting move before they write.
A project deleted through `deleteProjectLocal` clears its key's records once
no project the client holds wears that key; an archived, detached or
unlisted row keeps them. The fence does not see a write through `appStorage` under the same storage
key. `App.svelte` starts it and reloads
its records after hydration (`syncSidebarEntryDisclosureFromAppStorage`); the
test setup starts it and resets it, records included. Nothing migrates what a
project id was the key of: an id cannot become an entry key without every
computer's project rows, which the store has not read when it initializes.
Separate records are what makes that state unreachable rather than unread,
because an entry's key moves with its repository identity and a shared record
would answer the id again the moment the identity went. Every project opens
once. The id-keyed fold is dropped in `syncSidebarFromAppStorage`, after
hydration, since the first write of a boot marks the client migrated and a
boot whose host read failed must still be able to read the host bucket. Do not add permanent
frontend tombstones for moved or retired entities; pending reads hold only
enough ownership evidence to reject stale replies.

## The ThreadPane modules

Keep `thread.svelte.ts` as the pane facade. Put timeline windows and pagination
in `threadTimelineWindow.svelte.ts`, item merging in `threadItems.ts` and
`threadItemUpserts.ts`, stream application in `threadItemStreamApply.ts`, turn projection in `threadPaneTurns.svelte.ts`, and scrolling
in `threadPaneScroll.svelte.ts`. Do not add another reactive copy of timeline
items. Detailed scroll contracts live in
[frontend-scroll.md](../../../../docs/architecture/frontend-scroll.md).

## The reveal invariant

While a smoother owns an assistant row, the published text is its reveal cursor.
Every wholesale replacement passes through `prepareItemReplacement` before
`commitTimelineItems` or `upsertItemsBatch`. A trailing persisted or replica
summary must not rewind the cursor or dispose its smoother. Two streaming
reads that carry `streamEnd` compare by position; otherwise, for
reasoning-tail rows, containment rather than prefix equality determines
whether a summary trails. A genuinely divergent authoritative summary wins and
may snap forward.

Provider completion and an empty reveal backlog do not release successor rows.
The message must be terminal and its smoother drained. Exercise reveal ordering
through `eventsItemStream`, because independent rows may be batched differently
even while each item's order is preserved.

A settled read that turns a revealing `thinking` row into `assistant_text`
under the same id (a Claude narration block) is not installed when it arrives.
`threadRowPromotions.svelte.ts` holds it: the pane keeps its own row until the
reveal has drained and a mounted timeline has presented the change, then the
held row installs through `upsertItemsBatch`. Until then the pane's items
describe what is on screen, the held id gates the reveal queue, and patches,
meta and later reads for the id go to the held row. `snapAllToReceived`
installs every held row at once.

## Draft and model writes

`composerDraftSnapshots` serializes draft persistence. A prepared send is a fixed
ordering boundary: admit its snapshot before clearing the composer, enqueue later
edits behind it, and never dispatch after preparation fails. Only the write that
ran and still matches may clear pending state. The empty-draft cleanup is a
boundary in the same order: writes behind it are sent only if it kept the
thread, nothing is ever sent for a thread it deleted, and the store on that
thread keeps its buffer for the replacement the composer materializes. Text a
client holds unsaved for a thread that is deleted under it, by its own cleanup
or another screen's, moves to a new draft thread of its own and is never
dropped; a delete refused because another screen saved first leaves that
screen's draft alone, and so does a delete that failed or whose kept draft
could not be read. Controls that write a thread row wait for an in-flight
cleanup with `settleEmptyDraftCleanup` and act on what it leaves.
Hydration preserves attachment ID order and treats a missing attachment record
as an error.

`threadModelControls.ts` owns model changes from pickers and commands. Retry a
fallback with `UpdateThreadModelSelection`; confirmation comes from the effective
model event, not an unchanged thread-row reply.
