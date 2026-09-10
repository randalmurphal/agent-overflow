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
the exact RPC scope before firing. Mount effects may rely on scope reactivity,
but one-shot startup work must await `pageGrantsResolved()`. Suppress only typed
passive connection failures with `isPassiveConnectionFailure`; writes, explicit
actions, scope errors, and file errors remain visible.

One thread may be mounted in only one pane. Enter through `mountThreadInPane`;
`replaceThreadInPane` stays private. All writes to a pane's thread identity go
through `assignThread`, which refreshes transport watches. Use `revealPane` for
actions that expose a pane so compact layout moves to the thread screen.

## Events and convergence

`events.ts` is the single subscription root. Add a channel there and put its
reaction in the matching `events*.ts` module. Every event includes its origin as
the second `wailsEventOn` argument; an empty `backendId` means unknown origin.

Treat persisted row events as distributed convergence:

- `thread:updated` and `project:updated` apply the complete row for `full` and
  `listed`; `unlisted` and `deleted` remove it. A thread `full` frame never adds
  unknown sidebar membership. A `patch` changes only its named fields.
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

Live turn, approval, user-input and compacting state is backend state, never
derived from items or rows. `threadLiveActivity.ts` reads
`ListThreadLiveActivity` for every thread of a computer on each connection
edge and after a gap on one of those channels; a pane additionally reads
`GetThreadLiveState` when it mounts. A push that lands while a snapshot is in
flight wins over the snapshot.

`watchedThreads.ts` unions registered sources for every thread whose surface
exists, including child threads with no pane. Watches never depend on focus,
visibility, or `document.hidden`. Registering a consumer of an entity-filtered
channel also requires contributing its thread IDs. Push the opening watch set
before history loads, and restate it whenever a pane adopts or clears a thread.

`screenPresence.ts` reports focus and visible panes only so the backend can
suppress redundant OS notifications. Do not use presence to govern
subscriptions, delivery, fetching, rendering, or other work.

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

Frontend preferences and appearance libraries remain local to the frontend and
survive host removal. Mirror only generated `FRONTEND_DEVICE_SETTINGS_KEYS` to
computer device buckets, without holding a local save open for an unavailable
computer. Appearance file locality is decided only after grants resolve.

Thread and project catalogs from several computers merge by repository identity,
never by filesystem path. Do not add permanent frontend tombstones for moved or
retired entities; pending reads hold only enough ownership evidence to reject
stale replies.

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
summary must not rewind the cursor or dispose its smoother. For reasoning-tail
rows, containment rather than prefix equality determines whether a summary
trails. A genuinely divergent authoritative summary wins and may snap forward.

Provider completion and an empty reveal backlog do not release successor rows.
The message must be terminal and its smoother drained. Exercise reveal ordering
through `eventsItemStream`, because independent rows may be batched differently
even while each item's order is preserved.

## Draft and model writes

`composerDraftSnapshots` serializes draft persistence. A prepared send is a fixed
ordering boundary: admit its snapshot before clearing the composer, enqueue later
edits behind it, and never dispatch after preparation fails. Only the write that
ran and still matches may clear pending state. Hydration preserves attachment ID
order and treats a missing attachment record as an error.

`threadModelControls.ts` owns model changes from pickers and commands. Retry a
fallback with `UpdateThreadModelSelection`; confirmation comes from the effective
model event, not an unchanged thread-row reply.
