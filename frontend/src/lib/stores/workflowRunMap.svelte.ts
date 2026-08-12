// The run map's data plane: one whole run TREE per watched run, kept live by
// patching the `workflow:*` events into it and refetching whenever a patch
// cannot be placed exactly (RUN-MAP §4.4).
//
// Entity-keyed on `entityStore.svelte.ts` rather than pushed into a plain
// registry, because there IS something to release: `WorkflowGetRunMap` is a
// staleable RPC answer that has to be re-acquired across a transport
// reconnect, and the primitive's suspend/resetAll edge is the only reason this
// store has no reconnect story of its own to get wrong (the overlay's older
// run cache has none at all — that is the bug this must not inherit).
//
// The key is the item id the UI ASKED for — the nav-stack run id — not the
// tree root it resolves to. The ANSWER always covers the whole tree whichever
// member was named, so one entry serves the wave, the root and every run below
// either of them. The root is server-side (§5.9), so the frontend
// cannot know it before the first answer, and keying on something the caller
// cannot state would mean an attach that has to wait for a fetch to learn its
// own key. Two keys naming one tree (a deep link to a child plus the root)
// are two entries holding two copies; that is the same shape gitStatusStore
// carries for two spellings of one directory, and it costs one extra RPC in a
// case that only arises when both surfaces are open at once.
//
// PATCHES ARE AN OPTIMIZATION, NEVER LOAD-BEARING FOR CORRECTNESS. Every event
// carries strictly less than the view does — a parked attempt's engine cause, a
// run's endedAt, the tree's rolled-up spend are all absent from the wire
// payloads — so anything an event does not state exactly goes to a debounced
// `invalidate`, which re-sources while KEEPING the last value (no flicker,
// ~200ms late). WHICH events those are lives in `workflowRunMapPatch.ts`, pure
// and table-tested; this module owns the entry, the routing index and the
// debounce, and does exactly what that module's verdict says.
//
// A REFUSAL (§4.2) is data, not failure. `WorkflowGetRunMap` answers a
// permanently-unanswerable map by SUCCEEDING with `runs: []` and a refusal
// code, precisely so the primitive's backoff ladder — the right answer to a
// transient failure and the wrong one to a permanent refusal — never starts.
// It therefore applies like any other view, and the only thing this module owes
// it is not to drag it back into the event-driven refetch path.

import { WorkflowGetRunMap } from './bindings';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';
import {
  isTerminalRunState,
  patchItemState,
  patchPhaseState,
  patchSoftStop,
  type PatchResult,
} from './workflowRunMapPatch';
import type {
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
  WorkflowRunMapView,
  WorkflowSoftStopEvent,
} from '../types/workflow';

/**
 * How long an event that needs reconciling waits before the key it belongs to
 * re-sources. One window for every reason to invalidate: a burst that ends in a
 * refetch is one refetch whether it was five ambiguous units, one unknown
 * child, or thirty-two units that each just acquired a thread.
 *
 * Exported for the store's own tests, which must advance exactly this window —
 * a restated literal there is a second place the number lives.
 */
export const INVALIDATE_DEBOUNCE_MS = 200;

// ---------------------------------------------------------------------------
// The tree-member index
// ---------------------------------------------------------------------------

// Every run in a watched view → the keys whose view contains it. Events are
// addressed by ITEM id and a map is addressed by the id the UI asked for, so
// without this a phase event for wave 7 of a campaign could not find the root
// entry that draws it.
//
// The index belongs to the ENTRY, not to the source run that last filled it: it
// is rebuilt by every apply (the view is the only authority on what its tree
// contains) and released only when the entry leaves the store. Releasing it on
// a re-source instead left the key DARK for the whole width of the replacement
// fetch — every event for the tree hit an empty watcher list and vanished, and
// a run that parked inside that window was drawn as running until something
// unrelated refetched. Nothing else is acquired here, so a re-source has
// nothing to release at all.
const keysByMember = new Map<string, Set<string>>();
const membersByKey = new Map<string, string[]>();

function indexView(key: string, view: WorkflowRunMapView): void {
  forgetKey(key);
  const members: string[] = [];
  for (const run of view.runs) {
    if (!run.itemId) continue;
    members.push(run.itemId);
    let keys = keysByMember.get(run.itemId);
    if (!keys) {
      keys = new Set<string>();
      keysByMember.set(run.itemId, keys);
    }
    keys.add(key);
  }
  membersByKey.set(key, members);
}

function forgetKey(key: string): void {
  const members = membersByKey.get(key);
  if (!members) return;
  membersByKey.delete(key);
  for (const member of members) {
    const keys = keysByMember.get(member);
    if (!keys) continue;
    keys.delete(key);
    if (keys.size === 0) keysByMember.delete(member);
  }
}

function keysWatching(itemId: string): string[] {
  const keys = keysByMember.get(itemId);
  return keys ? [...keys] : [];
}

/**
 * Does this view name exactly the runs the key is already indexed for, in the
 * same order? A patch rebuilds one run inside the same array, so the answer is
 * yes for the high-rate path and the reindex is skipped. A different ORDER
 * answers no and reindexes — the walk is what the index is FOR, and paying for
 * it on a reshuffle that cannot happen is cheaper than reasoning about whether
 * it can.
 */
function sameMembers(key: string, view: WorkflowRunMapView): boolean {
  const members = membersByKey.get(key);
  if (!members) return false;
  let position = 0;
  for (const run of view.runs) {
    if (!run.itemId) continue;
    if (members[position] !== run.itemId) return false;
    position += 1;
  }
  return position === members.length;
}

// ---------------------------------------------------------------------------
// Debounced reconciliation
// ---------------------------------------------------------------------------

const pendingInvalidations = new Set<string>();
let invalidateTimer: ReturnType<typeof setTimeout> | null = null;

/**
 * Re-source a key because an event could not be placed in its view. Coalesced
 * across the burst: a fan-out that expands 32 units at once must cost one
 * refetch, not 32, and the events that follow inside the window are answered
 * by the fetch already on its way.
 */
function invalidateSoon(key: string): void {
  pendingInvalidations.add(key);
  if (invalidateTimer !== null) return;
  invalidateTimer = setTimeout(() => {
    invalidateTimer = null;
    const keys = [...pendingInvalidations];
    pendingInvalidations.clear();
    // invalidate() is a no-op for a key nobody holds any more, so a run the
    // user navigated away from during the window costs nothing.
    for (const key of keys) store.invalidate(key);
  }, INVALIDATE_DEBOUNCE_MS);
}

function cancelPendingInvalidations(): void {
  pendingInvalidations.clear();
  if (invalidateTimer === null) return;
  clearTimeout(invalidateTimer);
  invalidateTimer = null;
}

// ---------------------------------------------------------------------------
// The fetch race
// ---------------------------------------------------------------------------

// How many source runs are currently awaiting an answer for a key, and which
// keys saw an event while one was.
//
// A fetch's answer is a snapshot of the tree as it was when the READ ran, and
// an event that lands while it is in the air describes something that happened
// after. Applying the answer on top of the patch silently reverts it — and for
// the events that patch WITHOUT reconciling (a park writes a reason, not a
// terminal state, so nothing schedules a refetch behind it), the map then shows
// the pre-fetch truth until something unrelated happens to re-source. A counter
// rather than a flag because a superseded run and its replacement are both in
// flight for the same key, and the first to settle must not clear the mark the
// second is still racing.
const fetchesInFlight = new Map<string, number>();
const racedDuringFetch = new Set<string>();
/**
 * Which generation of the counters a fetch was counted into.
 *
 * The counter is balanced by a `finally`, and the reset seam clears the map out
 * from under fetches that have not settled yet — so the `finally` of a fetch
 * begun before a reset would decrement a counter belonging to a fetch begun
 * after it, drop that key to zero, and silently stop `markRacedFetch` from
 * recording anything for the rest of the live fetch. A stale `endFetch` is a
 * no-op instead: the counters it was balancing no longer exist.
 */
let fetchEpoch = 0;

function beginFetch(key: string): number {
  fetchesInFlight.set(key, (fetchesInFlight.get(key) ?? 0) + 1);
  return fetchEpoch;
}

function endFetch(key: string, epoch: number): void {
  if (epoch !== fetchEpoch) return;
  const outstanding = (fetchesInFlight.get(key) ?? 0) - 1;
  if (outstanding > 0) fetchesInFlight.set(key, outstanding);
  else fetchesInFlight.delete(key);
}

/** Note that this key just took a patch a fetch already in the air may bury. */
function markRacedFetch(key: string): void {
  if (fetchesInFlight.has(key)) racedDuringFetch.add(key);
}

/**
 * Consumed by the apply that lands: it is the one that may have buried it.
 *
 * A REFUSED answer consumes the mark without re-asking. Every refusal code is
 * permanent (§4.2), so the refetch could only produce the same refusal — this
 * is the one path that could still walk a refused key back into the
 * event-driven refetch loop the `isRefused` guard on the item-state path exists
 * to keep it out of, and the asymmetry between the two was unstated.
 */
function reconcileRacedFetch(key: string): void {
  if (!racedDuringFetch.delete(key)) return;
  if (isRefused(key)) return;
  invalidateSoon(key);
}

// ---------------------------------------------------------------------------
// The store
// ---------------------------------------------------------------------------

// Nothing is acquired beyond the answer itself: the map rides the broadcast
// `workflow:*` channel, so there is no per-key subscribe RPC and a source run
// has literally nothing to release. The routing index is the ENTRY's and comes
// off in `onDrop`; releasing it from a source cleanup made every re-source a
// window in which the key received no events at all. The entry is still the
// primitive's business — the retry curve, the refcount, and the transport edge
// are exactly what a hand-rolled fetch-on-mount would have had to reinvent.
const NOTHING_ACQUIRED = (): void => {};

const store = createEntityStore<WorkflowRunMapView, void>({
  name: 'workflowRunMap',
  // A view is a whole run tree — thousands of small objects for a long
  // campaign — and NOTHING writes into one: the patcher rebuilds the touched
  // run and reuses every other object, and consumers only ever re-project the
  // view wholesale. Deep proxying it would walk and wrap the entire tree on
  // first read to buy per-field tracking no consumer subscribes to.
  rawValue: true,
  source: async ({ key, apply, signal }) => {
    const epoch = beginFetch(key);
    let view: WorkflowRunMapView;
    try {
      view = (await WorkflowGetRunMap(key)) as WorkflowRunMapView;
    } finally {
      endFetch(key, epoch);
    }
    // A superseded run applies nothing (the primitive drops it) and must not
    // consume the race mark either — the replacement fetch is still in the air,
    // and the mark belongs to whichever answer actually lands.
    if (signal.aborted) return NOTHING_ACQUIRED;
    apply(view);
    reconcileRacedFetch(key);
    return NOTHING_ACQUIRED;
  },
  onApply: (key, value) => {
    // Most applies are PATCHES, which can only rewrite rows the tree already
    // had — never add or remove a run. Reindexing those walked the whole tree
    // and rebuilt two maps to arrive at exactly what was already there, on
    // every unit transition of a live fan-out.
    if (sameMembers(key, value)) return;
    indexView(key, value);
  },
  onDrop: (key) => {
    forgetKey(key);
    pendingInvalidations.delete(key);
    racedDuringFetch.delete(key);
  },
});

// ---------------------------------------------------------------------------
// Event entry points (routed from eventsWorkflow.ts)
// ---------------------------------------------------------------------------

/** Apply one verdict to one key. Reports whether the view actually changed. */
function applyPatch(key: string, result: PatchResult): boolean {
  if (result.kind === 'invalidate') {
    invalidateSoon(key);
    return false;
  }
  if (result.kind === 'unchanged') return false;
  // preserveError: a patch is a PARTIAL observation. It refreshes one node of
  // a view and says nothing about whether the fetch that would refresh the
  // rest has stopped failing, so it must not clear an error or cancel the
  // retry that is the key's only way back to a live source.
  store.apply(key, result.view, { preserveError: true });
  markRacedFetch(key);
  return true;
}

/**
 * Route one event to every key whose view contains the run it names.
 *
 * `reconcile` decides, per key, whether the debounced refetch runs behind the
 * patch — a predicate over what the patch DID rather than a flag, because the
 * two callers ask different questions of it: a `running` phase frame reconciles
 * whether or not it changed anything (the thread it cannot carry is attached
 * after the emit either way), while an item-state frame reconciles precisely
 * BECAUSE something moved that the payload underdetermines.
 */
function patchWatchers(
  itemId: string,
  patch: (view: WorkflowRunMapView) => PatchResult,
  reconcile: (changed: boolean) => boolean = () => false,
): void {
  for (const key of keysWatching(itemId)) {
    // Null while the store is suspended (disconnect blanks values but keeps
    // attachments). The reconnect resetAll re-sources every key, so there is
    // nothing here worth invalidating for.
    const view = store.snapshot(key);
    if (!view) continue;
    // A patch that landed can still be INCOMPLETE. The row flips now; the
    // debounced refetch behind it brings whatever the wire could not carry.
    if (reconcile(applyPatch(key, patch(view)))) invalidateSoon(key);
  }
}

/**
 * Did this key's last answer REFUSE (§4.2)? Every refusal code is permanent, so
 * a refused key must not be pulled back into an event-driven refetch loop —
 * the answer cannot change because a run in some other tree was born.
 */
function isRefused(key: string): boolean {
  const view = store.snapshot(key);
  return view !== null && (view.refusal?.code ?? '') !== '';
}

/**
 * `workflow:phase-state` — one attempt's or one unit's status moved.
 *
 * The high-rate path: a fan-out phase emits per unit, and those patch in
 * place. Anything the payload does not fully determine (an unknown unit, a
 * parked attempt's cause, a reopened row's attempt bump) reconciles.
 *
 * A `running` frame patches AND reconciles, whether or not the patch changed
 * anything. The thread a phase attempt or a unit runs in is attached AFTER the
 * engine emits it (the runner's `AttachWorkItemPhaseRun` /
 * `AttachWorkItemUnitRun`) and no event follows, so a row born while the map is
 * open would otherwise render as a running node nobody can click through to —
 * until something unrelated happened to refetch. `unchanged` is the WORST case
 * for that, not an exemption: it means a fetch already delivered the row as
 * running and threadless, so this frame is the only prompt anything has to go
 * back for the thread.
 */
export function applyWorkflowRunMapPhaseState(event: WorkflowPhaseStateEvent): void {
  if (!event || typeof event.itemId !== 'string' || event.itemId === '') return;
  const running = event.status === 'running';
  patchWatchers(event.itemId, (view) => patchPhaseState(view, event), () => running);
}

/**
 * `workflow:item-state` — a run's state moved.
 *
 * An id no watched view contains is reachable and meaningful in exactly one
 * shape: a child run's FIRST transition (`from: ""`) is how a new wave is
 * born, and the map that has to grow a wave is the one watching its parent.
 * There is no key to aim at, so every watched key reconciles — wave birth is
 * per-wave rare and a refetch is the right cost (§4.3.3).
 *
 * Every LATER transition of a run no view contains belongs to some other tree
 * entirely — the overlay is one surface over a whole project's runs, and the
 * channel is a broadcast. Refetching every open map because an unrelated run
 * parked is a refetch storm proportional to the project, not to the map.
 */
export function applyWorkflowRunMapItemState(event: WorkflowItemStateEvent): void {
  if (!event || typeof event.itemId !== 'string' || event.itemId === '') return;
  const watchers = keysWatching(event.itemId);
  if (watchers.length === 0) {
    // `from` is typed as a state, but birth is the one frame that carries none.
    if ((event.from as string) !== '') return;
    for (const key of store.keys()) {
      if (isRefused(key)) continue;
      invalidateSoon(key);
    }
    return;
  }
  // Every item-state frame that MOVED something reconciles, and the terminal
  // rule the module used to carry alone was one instance of that: state,
  // reason and the fields written WITH them travel together, and the payload
  // carries only the first two. `endedAt` is the standing example — a terminal
  // transition writes it, and so does a park, and so does `parkDisposition`,
  // which REWRITES it on a run that already had one. A take-over is the case
  // that has neither a state change nor an event of its own to follow: the
  // reason moves to `taken-over`, an intervention record is written on the
  // attempt, and no phase event is emitted (`engine/human.go`), so without this
  // the "touched by hand" marker never appeared until an unrelated refetch.
  //
  // What does NOT reconcile is a frame that restates the row exactly — a pure
  // running continuation. Those are the only ones the view already agrees with.
  patchWatchers(
    event.itemId,
    (view) => patchItemState(view, event),
    (changed) => changed || isTerminalRunState(event.to),
  );
}

/** `workflow:soft-stop` — the tree's standing stop request was armed/withdrawn. */
export function applyWorkflowRunMapSoftStop(event: WorkflowSoftStopEvent): void {
  if (!event || typeof event.itemId !== 'string' || event.itemId === '') return;
  if (typeof event.armed !== 'boolean') return;
  patchWatchers(event.itemId, (view) => patchSoftStop(view, event));
}

// ---------------------------------------------------------------------------
// Public surface
// ---------------------------------------------------------------------------

/**
 * Refcounted attach for one run's map. The key is the run id the UI is looking
 * at; the answer covers its whole tree whichever run in it was named.
 *
 * There is no ctx: the RPC needs the key and nothing else, and a ctx that
 * carried nothing would be a parameter every consumer had to invent a value
 * for.
 */
export function attachWorkflowRunMap(key: string): EntityAttachment<WorkflowRunMapView> {
  return store.attach(key, undefined);
}

/** Read a run's map without attaching. Reactive; null before the first answer. */
export function peekWorkflowRunMap(key: string | null): WorkflowRunMapView | null {
  return key === null || key === '' ? null : store.peek(key);
}

/** Read a run map's error without attaching. Reactive; null when healthy. */
export function peekWorkflowRunMapError(key: string | null): string | null {
  return key === null || key === '' ? null : store.peekError(key);
}

/** Diagnostics / tests: the run ids currently held. */
export function __workflowRunMapKeysForTest(): string[] {
  return store.keys();
}

/**
 * Transport-gap recovery: re-source every held map.
 *
 * `workflow:*` is edge-triggered — one frame per transition — so a dropped
 * frame is terminal for the view it belonged to: no later frame restates it,
 * and a map that missed a phase completion is indistinguishable from one whose
 * phase is genuinely still running. The gap signal carries no item id, so
 * recovery is blanket; live keys are bounded by the open overlay and the
 * re-source keeps each key's last value, so this costs a couple of RPCs and no
 * flicker.
 */
export function resyncWorkflowRunMapAfterGap(): void {
  // The refetch about to happen is a superset of every pending patch-failure
  // refetch, and firing them afterwards would re-source keys that just did.
  cancelPendingInvalidations();
  store.invalidateAll();
}

// ---------------------------------------------------------------------------
// Test seam
// ---------------------------------------------------------------------------

/**
 * Drop every entry and index, as a fresh module load would. suspend() releases
 * every unheld entry; resetAll() then lifts the suspension. An entry that
 * survives both is one a test attached and never released — resetAll
 * re-sources it against the next test's binding mocks, which is exactly the
 * noise that should make the leak findable.
 */
export function __resetWorkflowRunMapStoreForTest(): void {
  cancelPendingInvalidations();
  keysByMember.clear();
  membersByKey.clear();
  // Retire every outstanding fetch's claim on the counters BEFORE clearing
  // them, and strictly before resetAll re-sources: a fetch still in the air
  // has a `finally` that will decrement, and it must not decrement a count
  // belonging to a fetch this reset is about to start. One dropped to zero
  // that way silently stopped recording race marks for a live key.
  fetchEpoch += 1;
  fetchesInFlight.clear();
  racedDuringFetch.clear();
  store.suspend();
  store.resetAll();
}
