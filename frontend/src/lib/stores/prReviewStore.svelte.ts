// Live pull-request state, keyed by PR.
//
// Doctrine (frontend/CLAUDE.md → State Boundaries): state is keyed by its
// ENTITY. A pull request is one entity — its detail, review threads, head
// SHA, CI pipeline, and merge-conflict tree describe the PR, not the pane
// looking at it. Two panes reviewing one PR (a worktree thread and the
// pr-anchor thread it spawned; a split view of the same thread) therefore
// observe one snapshot, one poll pump, and one merge-tree computation
// instead of private copies that drift apart.
//
// This module owns the POLLED snapshot. The two caches derived from the
// same entity but sourced separately — the CI pipeline
// (`prReviewCI.svelte.ts`) and the merge-conflict tree
// (`prReviewConflicts.svelte.ts`) — live next door and are dropped from
// here through the entity store's `onDrop`, so neither runs a second
// refcount beside the primitive's.
//
// What stays with the pane: the diff it loaded, the head it loaded that
// diff AT (staleness is derived from the two — see `prStale` in
// reviewPane.svelte.ts), collapse/expansion, drafts, and the CI log view.
//
// The backend pumps one `pr:updated` stream per PR key and addresses its
// events by that key, so nothing here has to route by subscription id.

import { SvelteMap } from 'svelte/reactivity';
import {
  SetPRUpdatesActive,
  SubscribePRUpdates,
  UnsubscribePRUpdates,
} from './bindings';
import { createEntityStore } from './entityStore.svelte';
import { isTransportClassError, onTransportStatusChange } from './transportStatus.svelte';
import { __resetPRCIForTest, dropPRCI, hasPRCI, loadPRCIJobs } from './prReviewCI.svelte';
import {
  __resetPRConflictsForTest,
  dropPRConflicts,
  reconcileConflictsWithHead,
} from './prReviewConflicts.svelte';
import { documentHidden } from '../utils/pageVisibility';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import type { PRDetail, ReviewThread } from '../types/models';

/** The polled half of a PR: what `SubscribePRUpdates` observes. */
export interface PRSnapshot {
  readonly detail: PRDetail | null;
  readonly threads: readonly ReviewThread[];
  /** The PR's LIVE head OID — not the head any pane's diff was loaded at. */
  readonly headSHA: string;
}

/** What a source needs from whoever is holding the key. */
export interface PRCtx {
  ref: PRRef;
}

// Wire payload shape for "pr:updated". Wails generates no TS type for
// event payloads, so the shape is declared locally and kept in sync with
// PRUpdatedEvent in app_forge_review.go.
export interface PRUpdatedEvent {
  prKey: string;
  detail?: PRDetail | null;
  threads?: ReviewThread[] | null;
  headSHA?: string;
  /** A fetch failure on the pump: user-facing state, not a log line. */
  error?: string;
  /**
   * The pump-state sequence this frame was stamped with, comparable against
   * the one a `SubscribePRUpdates` result carries. See the join replay
   * buffer below.
   */
  seq?: number;
}

// ---------------------------------------------------------------------------
// Wire-key aliasing
// ---------------------------------------------------------------------------

// The backend's PR key (`prUpdateKey` in app_forge_review.go) and this
// frontend's (`prKey` in utils/prReference.ts) are built to be the same
// string, and for every real PR they are. They are not GUARANTEED to be:
// Go joins namespace and repo through `PRReference.Project()`, TypeScript
// through a literal `${namespace}/${repo}`, and an empty namespace already
// makes the two disagree (`github:repo:5` vs `github:/repo:5`). Rather
// than assume byte-identity between two independently-written formatters,
// the subscribe result's key is recorded as an ALIAS of the local key and
// `pr:updated` routes through the map — the same shape gitStatusStore uses
// for canonical cwds.
//
// The owner stamp is what makes it safe under re-sourcing: a superseded
// run (invalidate, reconnect, retry) resolves late and then runs its own
// cleanup; without ownership that cleanup deletes the alias the live run
// installed, and the key goes on holding a subscription whose pushes route
// nowhere.
type AliasOwner = symbol;
const localKeysByWireKey = new Map<string, Map<string, AliasOwner>>();

function addAlias(wireKey: string, key: string, owner: AliasOwner): void {
  let keys = localKeysByWireKey.get(wireKey);
  if (!keys) {
    keys = new Map<string, AliasOwner>();
    localKeysByWireKey.set(wireKey, keys);
  }
  keys.set(key, owner);
}

function removeAlias(wireKey: string, key: string, owner: AliasOwner): void {
  const keys = localKeysByWireKey.get(wireKey);
  if (keys?.get(key) !== owner) return;
  keys.delete(key);
  if (keys.size === 0) localKeysByWireKey.delete(wireKey);
}

// ---------------------------------------------------------------------------
// The polled snapshot
// ---------------------------------------------------------------------------

// The ref every key was attached with, so pump-driven refreshes (CI) and
// conflict recomputes can issue their own calls without an attacher
// handing one in.
const refByKey = new Map<string, PRRef>();
// Live subscription id per key — at most one, because the entity store
// sources a key once however many panes hold it. Visibility flips address
// these, not the panes.
const subscriptionIdByKey = new Map<string, string>();

// ---------------------------------------------------------------------------
// The join → push handoff
// ---------------------------------------------------------------------------

// A subscribe reads the pump's state under the backend's mutex, but the
// alias that routes `pr:updated` here is installed only once the response
// gets home. A frame the pump emits inside that window routes to no key —
// and the pump only emits on CHANGE, so nothing re-states it: the pane
// shows the pre-join snapshot until the PR next moves. A SECOND key joining
// a wireKey the first one already routes has the same window, the frame
// simply reaching key 1 alone, so frames are buffered whether or not they
// routed.
//
// The seq watermark below is what makes replaying them safe. The backend
// stamps a frame in the same critical section that stored the state it
// carries, so anything at or below the seq the subscribe returned is ALREADY
// in that result; only a strictly greater seq is a frame the join provably
// missed.
//
// One slot per wireKey holds BOTH kinds of frame, because they carry
// different things and a snapshot must not be lost under a later error: the
// backend emits either a snapshot frame or an error-only one, so keeping
// just the newest replayed the error over the join's stale snapshot and left
// the observed data unstated until the PR next changed.
//
// Buffered only while a subscribe is in flight — steady state holds nothing,
// so a thread's payloads never linger past the window that could need them.
interface BufferedFrames {
  snapshot: PRUpdatedEvent | null;
  error: PRUpdatedEvent | null;
}
const bufferedFrameByWireKey = new Map<string, BufferedFrames>();
let joinsInFlight = 0;

// The highest seq applied per LOCAL key, seeded from the subscribe result.
// Ordering is per PR key on both sides: the backend stamps under one mutex
// and a dead pump stamps nothing, so seq order is content order.
const appliedSeqByKey = new Map<string, number>();

function beginPRUpdateJoin(): void {
  joinsInFlight += 1;
}

function endPRUpdateJoin(): void {
  joinsInFlight -= 1;
  if (joinsInFlight > 0) return;
  // Paired by the source's try/finally, so this is zero rather than
  // negative — and a closed window must hold no payloads.
  joinsInFlight = 0;
  bufferedFrameByWireKey.clear();
}

function bufferPRUpdateFrame(wireKey: string, event: PRUpdatedEvent): void {
  let slot = bufferedFrameByWireKey.get(wireKey);
  if (!slot) {
    slot = { snapshot: null, error: null };
    bufferedFrameByWireKey.set(wireKey, slot);
  }
  if (event.error) {
    slot.error = event;
    return;
  }
  // The pump clears its stored failure before emitting a successful poll, so
  // a snapshot frame supersedes every earlier error — which is exactly what
  // live routing would have left on the key.
  slot.snapshot = event;
  slot.error = null;
}

const store = createEntityStore<PRSnapshot, PRCtx>({
  name: 'prReview',
  source: async ({ key, getCtx, apply, signal }) => {
    const ref = getCtx().ref;
    const owner: AliasOwner = Symbol(key);
    // The join window opens before the RPC leaves and closes only once this
    // run has installed its alias and folded the result in — every frame in
    // between is a candidate for replay.
    beginPRUpdateJoin();
    try {
      let result;
      try {
        result = await SubscribePRUpdates(prReferenceWire(ref));
      } catch (err) {
        // The waiter is a pane's PR load: it must fail with the reason
        // rather than hang until the retry curve happens to succeed. Only
        // for the LIVE run, though — a superseded run's failure is not the
        // waiter's answer, and rejecting on it would fail a load that the
        // run which replaced it is about to satisfy.
        if (!signal.aborted) rejectReady(key, err);
        throw err;
      }
      const id = String(result.id);
      const wireKey = String(result.prKey || key);
      const cleanup = async (): Promise<void> => {
        removeAlias(wireKey, key, owner);
        if (subscriptionIdByKey.get(key) === id) subscriptionIdByKey.delete(key);
        try {
          await UnsubscribePRUpdates(id);
        } catch (err) {
          // A dead wire needs no unsubscribe: the backend releases every
          // subscription a connection held when it drops. Anything else is
          // a real failure to release and must be seen.
          if (!isTransportClassError(err)) throw err;
        }
      };
      // A run that lost its race publishes nothing: the id it just acquired
      // is not the key's live subscription, so a visibility flip must not
      // address it and a push routed to it belongs to a world that is gone.
      // Returning the cleanup releases it immediately — the entity store
      // runs it as soon as it sees the stale generation.
      if (signal.aborted) return cleanup;
      addAlias(wireKey, key, owner);
      subscriptionIdByKey.set(key, id);
      // The result IS this key's applied state, so it is the watermark every
      // frame from here on is ranked against — the replay below included.
      appliedSeqByKey.set(key, Number(result.seq ?? 0));
      // A load can finish while the window sits minimized; the fresh pump
      // must start paused like every other live one.
      if (documentHidden()) setPumpActive(id, false);
      const snapshot: PRSnapshot = {
        detail: (result.detail ?? null) as PRDetail | null,
        threads: (result.threads ?? []) as ReviewThread[],
        headSHA: String(result.headSHA ?? result.detail?.headSHA ?? ''),
      };
      apply(snapshot);
      // The pump's ACTIVE failure. Identical failures are deduped backend
      // side, so no frame will ever restate it for this subscriber. It does
      // not fail the load: stale data plus an error banner is what every
      // holder already on the key sees, and a joiner must see the same.
      if (result.error) store.applyError(key, new Error(String(result.error)));
      // Frames the join may have missed, applied to THIS key alone — any
      // sibling key routing the same wireKey already had them. Which of them
      // the result already accounts for is not decided here: they go through
      // the same watermark chokepoint as a live frame, seeded a few lines up
      // with the result's own seq. Snapshot before error, the order they
      // were observed in.
      const missed = bufferedFrameByWireKey.get(wireKey);
      if (missed?.snapshot) applyPRUpdateToKey(key, missed.snapshot);
      if (missed?.error) applyPRUpdateToKey(key, missed.error);
      resolveReady(key, snapshot);
      return cleanup;
    } finally {
      endPRUpdateJoin();
    }
  },
  onApply: (key, value) => {
    reconcileConflictsWithHead(key, refByKey.get(key), value.detail, value.headSHA);
    reconcileResolveOverrides(key, value.threads);
  },
  // The two caches derived from this entity but not sourced by it. They
  // hang off the primitive's one teardown hook instead of a second
  // refcount beside it — a hand-rolled one is a thing to keep in sync,
  // and the copy that drifts is the one that leaks.
  //
  // `onDrop` fires when an entry LEAVES the store, which is the last
  // release: suspend and resetAll only drop entries nobody holds. A
  // reconnect therefore keeps the CI rows and the merged tree of a PR
  // somebody is still reading, and the ready-deferreds — which a
  // disconnect DOES have to settle — are handled at the transport edge
  // below rather than here.
  onDrop: (key) => {
    refByKey.delete(key);
    appliedSeqByKey.delete(key);
    resolveOverridesByKey.delete(key);
    dropPRCI(key);
    dropPRConflicts(key);
    rejectReady(key, new Error('PR updates released'));
  },
});

// ---------------------------------------------------------------------------
// Resolve overrides (optimistic, anti-flap)
// ---------------------------------------------------------------------------

// A resolve/unresolve flips its button the moment it is clicked, but the
// poll pump may have a snapshot in flight that was fetched BEFORE the forge
// applied the mutation — applying it verbatim would flap the thread back
// for one poll interval. The override is the optimistic verdict, held at
// the PR entity (both panes on one PR agree) until a snapshot AGREES with
// it. It is not time-boxed: the backend read-back-verifies the mutation
// before answering, so the next successful poll observes the new state.
//
// Both maps are Svelte-reactive because the projection below runs inside
// panes' $deriveds: creating a key's first override must wake them.
const resolveOverridesByKey = new SvelteMap<string, SvelteMap<string, boolean>>();

/** Record the optimistic resolved state for one thread. */
export function setPRThreadResolveOverride(key: string, threadId: string, resolved: boolean): void {
  let overrides = resolveOverridesByKey.get(key);
  if (!overrides) {
    overrides = new SvelteMap<string, boolean>();
    resolveOverridesByKey.set(key, overrides);
  }
  overrides.set(threadId, resolved);
}

/** Drop one override — the RPC failed, so the forge state stands. */
export function clearPRThreadResolveOverride(key: string, threadId: string): void {
  resolveOverridesByKey.get(key)?.delete(threadId);
}

/**
 * Project a snapshot's threads through the live overrides. Identity-stable
 * when nothing is overridden: callers re-anchor the reader on prThreads
 * identity change, so a fresh array is minted only when it differs.
 */
export function overriddenPRThreads(
  key: string,
  threads: readonly ReviewThread[],
): readonly ReviewThread[] {
  const overrides = resolveOverridesByKey.get(key);
  if (!overrides || overrides.size === 0) return threads;
  let changed = false;
  const out = threads.map((thread) => {
    const want = overrides.get(thread.id);
    if (want === undefined || thread.isResolved === want) return thread;
    changed = true;
    return { ...thread, isResolved: want };
  });
  return changed ? out : threads;
}

// Runs at the apply chokepoint: an override whose thread the snapshot now
// agrees with has done its job, and one whose thread vanished (deleted on
// the forge) has nothing left to override.
function reconcileResolveOverrides(key: string, threads: readonly ReviewThread[]): void {
  const overrides = resolveOverridesByKey.get(key);
  if (!overrides || overrides.size === 0) return;
  const resolvedById = new Map(threads.map((thread) => [thread.id, thread.isResolved]));
  for (const [threadId, want] of [...overrides]) {
    const observed = resolvedById.get(threadId);
    if (observed === undefined || observed === want) overrides.delete(threadId);
  }
  if (overrides.size === 0) resolveOverridesByKey.delete(key);
}

// First-observation waiters. A PR load needs the detail's base ref before
// it can ask for a diff, so the pane awaits the first snapshot; every
// later pane on the same key reads the value that is already there.
//
// One deferred per key, not per source attempt: a reconnect re-sources
// the key and must settle the waiter that is still parked on it.
interface ReadyDeferred {
  promise: Promise<PRSnapshot>;
  resolve(value: PRSnapshot): void;
  reject(err: unknown): void;
}
const readyByKey = new Map<string, ReadyDeferred>();

function readyFor(key: string): Promise<PRSnapshot> {
  const existing = readyByKey.get(key);
  if (existing) return existing.promise;
  let resolve!: (value: PRSnapshot) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<PRSnapshot>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  // Mark the deferred itself handled: a rejection that lands before any
  // pane awaited it is an ordinary outcome here, not an unhandled error.
  promise.catch(() => {});
  readyByKey.set(key, { promise, resolve, reject });
  return promise;
}

// Resolves with the value the store is HOLDING, so every waiter — the first
// one and every later `ready()` that short-circuits on `handle.current` —
// observes the same reactive object. `applied` is the fallback for the one
// case where the store holds nothing: the entry was torn down while this
// source was in flight, so its apply was dropped by the generation guard.
function resolveReady(key: string, applied: PRSnapshot): void {
  const deferred = readyByKey.get(key);
  if (!deferred) return;
  readyByKey.delete(key);
  deferred.resolve(store.peek(key) ?? applied);
}

function rejectReady(key: string, err: unknown): void {
  const deferred = readyByKey.get(key);
  if (!deferred) return;
  readyByKey.delete(key);
  deferred.reject(err);
}

// A disconnect suspends the store, which means no source will run and no
// waiter will be settled by one. A PR load parked on `ready()` would sit
// there for the whole outage — a spinner with no explanation and no end,
// where the connection banner is already saying what happened. Fail them
// with that reason; a reconnect re-sources every held key, and the reload
// that follows parks on a fresh deferred the new source settles.
onTransportStatusChange((status) => {
  if (status.status === 'connected') return;
  // Sequences are the dead connection's, and the resubscribe that follows a
  // reconnect starts a fresh pump: a buffered frame outranking its result
  // would replay a snapshot from a world that is gone.
  bufferedFrameByWireKey.clear();
  for (const key of [...readyByKey.keys()]) {
    rejectReady(key, new Error('Disconnected from the backend.'));
  }
});

// ---------------------------------------------------------------------------
// Pump visibility
// ---------------------------------------------------------------------------

// Visibility votes are LAST-WRITE-WINS on the backend, but the transport
// dispatches concurrently: a hidden→visible flurry (a window minimised and
// restored, a tab flipped) can complete out of order and leave the pump
// holding `false` for a client that is on screen — a review pane that
// silently stops updating until the next visibility change. So one vote is
// in flight per subscription at a time, with the newest value trailing
// behind it; intermediate values are dropped, which is what "latest wins"
// means when the far side only cares about the final state.
interface PumpVote {
  sending: boolean;
  pending: boolean | null;
}
const pumpVotes = new Map<string, PumpVote>();

function setPumpActive(id: string, active: boolean): void {
  let vote = pumpVotes.get(id);
  if (!vote) {
    vote = { sending: false, pending: null };
    pumpVotes.set(id, vote);
  }
  vote.pending = active;
  if (vote.sending) return;
  void drainPumpVotes(id, vote);
}

async function drainPumpVotes(id: string, vote: PumpVote): Promise<void> {
  vote.sending = true;
  try {
    while (vote.pending !== null) {
      const active = vote.pending;
      vote.pending = null;
      try {
        await SetPRUpdatesActive(id, active);
      } catch (err) {
        // Not user-surfaced: a failed pause keeps the status quo (the pump
        // just keeps polling), and a failed resume implies a dying transport
        // whose server-side connection cleanup closes the pump anyway.
        console.error('prReview: SetPRUpdatesActive failed', { id, active, err });
      }
    }
  } finally {
    vote.sending = false;
    // Nothing arrived while the last send was in flight, so this
    // subscription owes no vote — drop the bookkeeping rather than keep an
    // entry per subscription the app ever made.
    if (vote.pending === null && pumpVotes.get(id) === vote) pumpVotes.delete(id);
  }
}

/**
 * A hidden window doesn't need PR polling. Each live subscription reports
 * its own visibility; the backend composes them, so one visible client
 * keeps a shared pump running for everyone.
 */
export function handlePRVisibilityChange(): void {
  const active = !documentHidden();
  for (const id of subscriptionIdByKey.values()) setPumpActive(id, active);
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', handlePRVisibilityChange);
}

// ---------------------------------------------------------------------------
// Write chokepoints
// ---------------------------------------------------------------------------

/**
 * Route a `pr:updated` push to its PR. The frame is addressed by the
 * BACKEND's key, which is resolved to the local key(s) subscribed through
 * it; applying to a key nobody holds is a no-op, so a late frame for a
 * closed pane resurrects nothing.
 */
export function applyPRUpdatedEvent(event: PRUpdatedEvent): void {
  const wireKey = event.prKey;
  if (!wireKey) return;
  if (joinsInFlight > 0) bufferPRUpdateFrame(wireKey, event);
  const keys = localKeysByWireKey.get(wireKey);
  if (!keys) return;
  for (const key of keys.keys()) applyPRUpdateToKey(key, event);
}

// The one place a frame reaches a key, live or replayed, and therefore the
// one place ordering is decided. A frame that lost a race with the subscribe
// result already accounting for it — or with a later frame — must not
// regress the entity: the pump emits only on CHANGE, so nothing restates
// what a stale frame overwrote until the PR itself moves.
//
// An unstamped frame applies unguarded. That is transition safety only —
// every frame the backend emits today carries a seq.
function applyPRUpdateToKey(key: string, event: PRUpdatedEvent): void {
  const seq = typeof event.seq === 'number' ? event.seq : null;
  if (seq !== null) {
    const applied = appliedSeqByKey.get(key);
    if (applied !== undefined && seq <= applied) return;
  }
  if (event.error) {
    store.applyError(key, new Error(event.error));
    if (seq !== null) appliedSeqByKey.set(key, seq);
    return;
  }
  const detail = (event.detail ?? null) as PRDetail | null;
  store.apply(key, {
    detail,
    threads: (event.threads ?? []) as ReviewThread[],
    headSHA: String(event.headSHA || detail?.headSHA || ''),
  });
  if (seq !== null) appliedSeqByKey.set(key, seq);
  // The pump only fires on snapshot change, so this tracks check/pipeline
  // movement without a poll of its own — once per PR, not once per pane.
  const ref = refByKey.get(key);
  if (ref && hasPRCI(key)) void loadPRCIJobs(key, ref);
}

/** Apply a freshly fetched detail + threads pair (comments-only refresh). */
export function applyPRSnapshot(key: string, snapshot: PRSnapshot): void {
  store.apply(key, snapshot);
}

/**
 * Apply re-listed review threads after a submit or reply, keeping the
 * detail and live head the pump last observed. Every pane on the PR heals.
 *
 * PARTIAL, deliberately: this observed the forge's review threads, which is
 * no evidence the poll pump recovered. Clearing the error here dismissed a
 * "PR updates stopped" banner that was still true, and the backend dedups
 * identical failures — so no later event would have restored it. Only a
 * successful pump observation (or a full-detail refresh) clears it.
 */
export function applyPRThreads(key: string, threads: readonly ReviewThread[]): void {
  const current = store.snapshot(key);
  store.apply(
    key,
    {
      detail: current?.detail ?? null,
      threads,
      headSHA: current?.headSHA ?? '',
    },
    { preserveError: true },
  );
}

export function applyPRError(key: string, err: unknown): void {
  store.applyError(key, err);
}

// ---------------------------------------------------------------------------
// Attach / read
// ---------------------------------------------------------------------------

export interface PRAttachment {
  readonly key: string;
  /** Reactive current snapshot; null before the first observation. */
  readonly snapshot: PRSnapshot | null;
  /** Reactive error; null when healthy. */
  readonly error: string | null;
  /**
   * The first observation. Resolves immediately when one is already in
   * hand, rejects when the subscribe fails, when the transport drops, or
   * when the last holder releases while it is still in flight — a load
   * waiting on it must fail, never hang.
   */
  ready(): Promise<PRSnapshot>;
  release(): void;
}

export function attachPR(key: string, ctx: PRCtx): PRAttachment {
  refByKey.set(key, ctx.ref);
  const handle = store.attach(key, ctx);
  let released = false;
  return {
    key,
    get snapshot() {
      return handle.current;
    },
    get error() {
      return handle.error;
    },
    ready() {
      const current = handle.current;
      return current ? Promise.resolve(current) : readyFor(key);
    },
    release() {
      if (released) return;
      released = true;
      // Everything a release has to drop hangs off the store's onDrop, so
      // the last-holder teardown cannot be forgotten by a new release path.
      handle.release();
    },
  };
}

/** Read a PR's snapshot without attaching. Reactive. */
export function peekPRSnapshot(key: string | null): PRSnapshot | null {
  return key === null ? null : store.peek(key);
}

/** Read a PR's error without attaching. Reactive; null when healthy. */
export function peekPRError(key: string | null): string | null {
  return key === null ? null : store.peekError(key);
}

/** Diagnostics / tests: the PRs currently held. */
export function prReviewKeys(): string[] {
  return store.keys();
}

/**
 * Transport-gap recovery: re-source every held PR.
 *
 * `pr:updated` only fires when the pump observes a CHANGED snapshot, so a
 * dropped frame is not followed by a corrective one — the review pane would
 * show a superseded head, stale check runs and missing comments until the PR
 * next moves. The gap signal carries no PR key, so recovery is blanket: live
 * keys are bounded by the open review panes, and re-sourcing keeps each key's
 * last snapshot, so nothing flickers while the fresh one loads.
 */
export function resyncPRReviewAfterGap(): void {
  store.invalidateAll();
}

// ---------------------------------------------------------------------------
// Test seam
// ---------------------------------------------------------------------------

/**
 * Drop every entry, subscription, and derived cache, as a fresh module load
 * would. suspend() releases every unheld entry; resetAll() then lifts the
 * suspension. An entry that survives both is one a test attached and never
 * released — resetAll re-sources it against the next test's binding mocks,
 * which is exactly the noise that should make the leak findable.
 */
export function __resetPRReviewStoreForTest(): void {
  store.suspend();
  store.resetAll();
  for (const key of [...readyByKey.keys()]) rejectReady(key, new Error('store reset'));
  refByKey.clear();
  subscriptionIdByKey.clear();
  localKeysByWireKey.clear();
  resolveOverridesByKey.clear();
  bufferedFrameByWireKey.clear();
  appliedSeqByKey.clear();
  pumpVotes.clear();
  // onDrop clears these for every key an entry existed for; a test that
  // loaded CI or opened conflicts without ever attaching has no entry to
  // drop, so the caches are reset directly too.
  __resetPRCIForTest();
  __resetPRConflictsForTest();
}
