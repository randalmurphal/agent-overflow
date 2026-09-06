import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
// Thread-row projection domain: syncing cached Thread rows (sidebar list +
// per-pane copies) against wire updates — read markers, latest-turn
// completion timestamps, activity bumps, durable proposed-plan /
// incomplete-turn status, and the thread:updated / mode-change channels.
// This is the shared leaf other events* modules import from; it never
// imports another events* module. Fan-in target of events.ts's
// setupEventListeners.
import type { TurnCompletedEvent } from '../types/events';
import type { Thread } from '../types/models';
import { closePanesShowingThread, findPaneShowingThread, iterPanes, syncThread } from './panes.svelte';
import { refreshProjects, touchProjectActivity } from './projects.svelte';
import { refreshThreadGroups } from './threadGroups.svelte';
import { addToast } from './toast.svelte';
import { getThreadById, getThreadLiveActivityAt, getThreads, readThreadRows, prependThread, removeThread, replaceAllThreads, replaceThread, touchThreadActivity } from './threads.svelte';
import { projectReaderMessageSent, projectThreadError } from './threadStatuses.svelte';
import type { ThreadPaneIngest } from './threadPaneRoles';
import { pendingLocalReadMarker } from './threadReadWrites';
import { deferEmptyDraftDeletion } from './emptyDraftCleanup';

// The registry hands out whole ThreadPanes; this module narrows them to
// the ingest surface at its two acquisition points, so a new pane member
// use here fails to compile until threadPaneRoles.ts lists it.
function ingestPanes(): Iterable<ThreadPaneIngest> {
  return iterPanes();
}

function ingestPaneShowingThread(threadId: string): ThreadPaneIngest | null {
  return findPaneShowingThread(threadId);
}

/**
 * Payload for the backend-emitted thread:mode_changed event. Mirrors
 * ThreadModeChangedEvent in app_thread_interaction_mode.go.
 */
export interface ModeChangedPayload {
  threadId: string;
  mode: NonNullable<Thread['mode']>;
  needsReconnect: boolean;
}

/**
 * Payload for thread:runtime_mode_changed — emitted whenever
 * UpdateThreadRuntimeMode persists a change. Runtime-mode changes restart
 * active sessions synchronously, so needsReconnect is false on success and
 * kept only for compatibility with the older event shape.
 */
export interface RuntimeModeChangedPayload {
  threadId: string;
  runtimeMode: Thread['runtimeMode'];
  needsReconnect: boolean;
}

/**
 * Merge a backend/wire thread row with newer local state. Read markers,
 * latest-turn completion, and activity timestamps only move forward
 * locally (ChatView's read-mark effect patches lastReadAt ahead of the
 * debounced MarkThreadRead persist), so a row snapshotted before that
 * persist landed must not drag them backward. Explicit unread (0) still
 * wins — see mergeReadMarkersPreservingUnread.
 */
function mergeThreadRowWithLocal(
  updated: Thread,
  // Batch callers (resyncThreadRows) pass a prebuilt id → Thread index
  // so an n-row resync is O(n) instead of n linear getThreadById scans.
  cachedThread: Thread | undefined = getThreadById(updated.id),
): Thread {
  // The wire value is kept apart from the local ones: `lastReadAt` is the
  // one field where the two are not interchangeable, because explicit
  // unread is the SMALLEST value it takes. See mergeReadMarker.
  const localReadMarkers: number[] = [];
  const latestCompletions = [updated.latestTurnCompletedAt];
  // getThreadLiveActivityAt folds in the live streaming box, so a row
  // snapshotted before recent stream beats catches the durable
  // updatedAt up to the newest live bump here.
  const activityMarkers = [updated.updatedAt, getThreadLiveActivityAt(updated)];
  if (cachedThread?.lastReadAt !== undefined) {
    localReadMarkers.push(cachedThread.lastReadAt);
  }
  if (cachedThread?.latestTurnCompletedAt !== undefined) {
    latestCompletions.push(cachedThread.latestTurnCompletedAt);
  }
  if (cachedThread && Number.isFinite(cachedThread.updatedAt)) {
    activityMarkers.push(cachedThread.updatedAt);
  }

  for (const pane of ingestPanes()) {
    if (pane.threadId !== updated.id || !pane.thread) continue;
    if (pane.thread.lastReadAt !== undefined) {
      localReadMarkers.push(pane.thread.lastReadAt);
    }
    if (pane.thread.latestTurnCompletedAt !== undefined) {
      latestCompletions.push(pane.thread.latestTurnCompletedAt);
    }
    if (Number.isFinite(pane.thread.updatedAt)) {
      activityMarkers.push(pane.thread.updatedAt);
    }
  }

  const lastReadAt = mergeReadMarker(updated.id, updated.lastReadAt, localReadMarkers);
  const latestTurnCompletedAt = mergeLatestTurnCompletedAt(latestCompletions);
  const updatedAt = mergeLatestActivityAt(activityMarkers);
  return { ...updated, updatedAt, lastReadAt, latestTurnCompletedAt };
}

export function syncThreadRow(updated: Thread): Thread {
  const merged = mergeThreadRowWithLocal(updated);
  syncThread(merged);
  return merged;
}

export function syncLatestTurnCompleted(evt: TurnCompletedEvent): void {
  const cachedThread = getThreadById(evt.threadId)
    ?? ingestPaneShowingThread(evt.threadId)?.thread;
  if (!cachedThread) {
    return;
  }
  const latestTurnCompletedAt = Math.max(
    cachedThread.latestTurnCompletedAt ?? Number.NEGATIVE_INFINITY,
    evt.completedAt,
  );
  syncThreadRow({
    ...cachedThread,
    latestTurnCompletedAt,
  });
}

export function syncThreadActivity(threadId: string, updatedAt: number): void {
  if (!threadId || !Number.isFinite(updatedAt)) return;
  // Live bumps land in per-entity boxes (threads/projects stores), not
  // in the row arrays or pane.thread — this runs on every streaming
  // flush, and replacing those objects here re-rendered the sidebar and
  // every pane.thread reader per beat. Row objects catch up via
  // mergeThreadRowWithLocal on the next full row sync.
  const thread = touchThreadActivity(threadId, updatedAt);
  let projectId = thread?.projectId;
  let latestUpdatedAt = Math.max(thread ? getThreadLiveActivityAt(thread) : 0, updatedAt);

  if (projectId === undefined) {
    for (const pane of ingestPanes()) {
      if (pane.threadId !== threadId || !pane.thread) continue;
      projectId = pane.thread.projectId;
      latestUpdatedAt = Math.max(latestUpdatedAt, pane.thread.updatedAt ?? 0);
      break;
    }
  }

  touchProjectActivity(projectId, latestUpdatedAt);
}

/**
 * Mid-session sidebar resync (transport-gap recovery). Unlike
 * refreshThreads' wholesale replacement — fine at boot, where no local
 * state exists yet — a resync races live local state in two directions:
 *
 *   - the snapshot's lastReadAt can predate the debounced MarkThreadRead
 *     persist for a read-mark the UI already applied, reverting a row
 *     the focused pane just cleared;
 *   - the snapshot can carry a completion (latestTurnCompletedAt) whose
 *     turn_completed event fell into the gap, which no pane ever saw.
 *
 * Rows therefore go through the same local-state merge as pushed
 * thread:updated rows, and panes showing a thread converge on the merged
 * copy. The pane fan-out is load-bearing: ChatView's read-mark effect
 * keys off pane.thread, so without it a gap-lost completion leaves the
 * sidebar "Completed" pill stuck on a thread the user is viewing.
 */
async function resyncThreadRows(): Promise<void> {
  let rows: Thread[];
  try {
    rows = await readThreadRows();
  } catch (err) {
    if (isPassiveConnectionFailure(err)) return;
    console.error('Failed to resync threads after transport gap:', err);
    addToast('error', 'Failed to load threads');
    return;
  }
  reconcileThreadRows(rows);
}

export function reconcileThreadRows(rows: Thread[]): void {
  const cachedById = new Map(getThreads().map((thread) => [thread.id, thread]));
  const merged = rows.map((row) => mergeThreadRowWithLocal(row, cachedById.get(row.id)));
  replaceAllThreads(merged, false);
  const mergedById = new Map(merged.map((thread) => [thread.id, thread]));
  for (const pane of ingestPanes()) {
    if (!pane.threadId || !pane.thread) continue;
    const row = mergedById.get(pane.threadId);
    if (row) pane.replaceThread(row);
  }
}

export function refreshSidebarProjections(): void {
  void resyncThreadRows();
  void refreshProjects();
  // Groups have no per-row merge to do (nothing local ever runs ahead of
  // the backend on them), so the boot-time wholesale load is also the
  // correct gap recovery.
  void refreshThreadGroups();
}

/**
 * Reconcile a thread row's read marker: the value the backend just sent
 * against the copies this page load is holding.
 *
 * Three rules, in order, and each one is a different question.
 *
 * 1. A read marker THIS page load is currently writing wins outright,
 *    value and all (`threadReadWrites.ts`). It is the newest thing that
 *    happened to the field and the backend has not answered for it yet,
 *    so no wire row can be later. This is the only rule that can return
 *    an explicit unread, and holding the claim is what earns it.
 * 2. Otherwise a wire 0 wins, because it IS the backend's answer and
 *    every local write has been settled by rule 1. Without this the
 *    field is forward-only and no client but the one that pressed the
 *    button ever sees a thread go unread.
 * 3. Otherwise the newest of everything defined. Local copies can lead
 *    the wire by a debounce interval, and a row snapshotted before the
 *    persist landed must not drag a read thread back to unread.
 *
 * Rule 3 used to be the whole function with "any 0 wins" bolted in front
 * of it, which made a cached 0 permanent: it was folded back into every
 * later merge and absorbed the timestamp another device's read
 * broadcast, for the life of the page.
 */
function mergeReadMarker(
  threadId: string,
  wireReadMarker: number | undefined,
  localReadMarkers: number[],
): number | undefined {
  const pending = pendingLocalReadMarker(threadId);
  if (pending.held) {
    return pending.lastReadAt;
  }
  if (wireReadMarker === 0) {
    return 0;
  }
  if (wireReadMarker === undefined && localReadMarkers.length === 0) {
    return undefined;
  }
  return Math.max(wireReadMarker ?? Number.NEGATIVE_INFINITY, ...localReadMarkers);
}

function mergeLatestTurnCompletedAt(completions: Array<number | undefined>): number | undefined {
  const definedCompletions = completions.filter((value): value is number => value !== undefined);
  if (definedCompletions.length === 0) {
    return undefined;
  }
  return Math.max(...definedCompletions);
}

function mergeLatestActivityAt(activityMarkers: Array<number | undefined>): number {
  const definedActivity = activityMarkers.filter((value): value is number =>
    value !== undefined && Number.isFinite(value));
  if (definedActivity.length === 0) return 0;
  return Math.max(...definedActivity);
}

// Shared patch-everywhere: updates the cached thread-list row (sidebar)
// and every pane currently showing the thread with the same partial
// patch. `applyModeChanged`, `applyRuntimeModeChanged`, and
// `updateThreadUsageCache` all repeat this shape; `getThreadById` is the
// same lookup as `getThreads().find((t) => t.id === threadId)`.
// `patchThreadDurableStatus` is deliberately NOT rewritten onto this
// helper — its no-op dedupe semantics (skip the replace when the patch
// doesn't actually change the thread) are documented as different.
function patchThreadEverywhere(threadId: string, patch: Partial<Thread>): void {
  const existing = getThreadById(threadId);
  if (existing) {
    replaceThread({ ...existing, ...patch });
  }
  for (const pane of ingestPanes()) {
    if (pane.threadId !== threadId || !pane.thread) continue;
    pane.replaceThread({ ...pane.thread, ...patch });
  }
}

export function updateThreadUsageCache(threadId: string, raw: string): void {
  patchThreadEverywhere(threadId, { lastTokenUsage: raw });
}

export function patchThreadDurableStatus(
  threadId: string,
  patch: Pick<Partial<Thread>, 'hasActionableProposedPlan' | 'hasIncompleteTurn'>,
): void {
  // No-op dedupe: skip the replace when none of the patch fields actually
  // change the thread. Callers are the turn-lifecycle handlers, which fire
  // on every round; without the dedupe a restated value STILL replaces
  // `pane.thread` with a new reference, triggering a reactive cascade
  // through any component that reads `pane.thread` directly. The durable
  // plan flag's other writer is the backend, which sends the whole row on
  // `thread:updated` whenever a proposed-plan write moves it.
  const existing = getThreads().find((thread) => thread.id === threadId);
  if (existing && !patchMatchesThread(existing, patch)) {
    replaceThread({ ...existing, ...patch });
  }
  for (const pane of ingestPanes()) {
    if (pane.threadId !== threadId || !pane.thread) continue;
    if (patchMatchesThread(pane.thread, patch)) continue;
    pane.replaceThread({ ...pane.thread, ...patch });
  }
}

function patchMatchesThread(
  thread: Thread,
  patch: Pick<Partial<Thread>, 'hasActionableProposedPlan' | 'hasIncompleteTurn'>,
): boolean {
  if (
    patch.hasActionableProposedPlan !== undefined
    && thread.hasActionableProposedPlan !== patch.hasActionableProposedPlan
  ) {
    return false;
  }
  if (
    patch.hasIncompleteTurn !== undefined
    && thread.hasIncompleteTurn !== patch.hasIncompleteTurn
  ) {
    return false;
  }
  return true;
}

/**
 * Payload for thread:updated. Mirrors triage.ThreadUpdateEvent, which owns
 * the action vocabulary; `action` names what this client must DO with the
 * row, because sidebar membership is not derivable from the row alone.
 *
 * Every persisted thread-row mutation sends one, which is what makes a
 * second attached client converge without a refresh. The client that issued
 * the mutation may also have applied its RPC result optimistically; the
 * broadcast row IS that RPC's return value, so the echo lands on state the
 * optimistic apply already reached rather than moving it somewhere else.
 */
export interface ThreadUpdateEvent {
  /** 'full' | 'patch' | 'listed' | 'unlisted' | 'deleted' */
  action: string;
  thread?: Thread;
  id?: string;
  title?: string;
  model?: string;
  sessionRef?: string;
  /**
   * A sidebar-activity bump from an activity-counting user_text persist,
   * and nothing else — turn completions and approval requests announce
   * themselves on their own channels. Deliberately NOT merged into the row:
   * see the patch branch below.
   */
  updatedAt?: number;
}

/**
 * Payload for thread:error_notice. Mirrors triage.ThreadErrorNoticeEvent:
 * ids only, because the error's prose stays on the item row.
 */
export interface ThreadErrorNoticeEvent {
  threadId?: string;
  itemId?: string;
}

export function applyThreadErrorNotice(evt: ThreadErrorNoticeEvent): void {
  if (!evt?.threadId) return;
  projectThreadError(evt.threadId);
}

export function applyThreadUpdated(evt: ThreadUpdateEvent): void {
  if (!evt) return;
  switch (evt.action) {
    case 'patch': {
      if (!evt.id) return;
      // `updatedAt` is applied WITHOUT a cached row, unlike the field
      // merges below. It carries the reader's own message landing on a
      // thread, which is a sidebar-ordering and attention-badge fact about
      // threads this client may have no row for yet; both effects self-guard
      // on an unknown id (touchThreadActivity no-ops for a thread the list
      // doesn't hold, and the badge clears are Set deletes). Merging it into
      // the row instead would replace the row object on every message — the
      // reason live activity lives in a keyed box in the first place.
      if (evt.updatedAt !== undefined) {
        syncThreadActivity(evt.id, evt.updatedAt);
        projectReaderMessageSent(evt.id);
      }
      if (evt.title === undefined && evt.model === undefined && evt.sessionRef === undefined) {
        // Nothing to merge. Falling through would still run syncThreadRow,
        // which folds live activity back into the row's own updatedAt and
        // replaces the object — per-beat churn on the activity-only patch,
        // for a copy with no changed field in it.
        return;
      }
      const cached = getThreadById(evt.id)
        ?? ingestPaneShowingThread(evt.id)?.thread;
      if (!cached) return;
      const merged = { ...cached };
      if (evt.title !== undefined) merged.title = evt.title;
      if (evt.model !== undefined) merged.model = evt.model;
      if (evt.sessionRef !== undefined) merged.sessionRef = evt.sessionRef;
      syncThreadRow(merged);
      return;
    }
    case 'deleted': {
      // The row is gone from SQLite. Same teardown the deleting client
      // runs on its own RPC result: drop the row and its per-thread
      // caches, then close panes that were showing it — a pane on a row
      // that no longer exists cannot load, send, or resume.
      if (!evt.id) return;
      removeThread(evt.id);
      const id = evt.id;
      if (!deferEmptyDraftDeletion(id, () => closePanesShowingThread(id))) closePanesShowingThread(id);
      return;
    }
    case 'unlisted': {
      // Archived: still in SQLite, no longer in the active sidebar. The
      // row is carried so a pane showing it converges before the pane
      // closes, which is the order the archiving client's own sequence
      // produces.
      const id = evt.thread?.id ?? evt.id;
      if (!id) return;
      if (evt.thread) syncThreadRow(evt.thread);
      removeThread(id);
      closePanesShowingThread(id);
      return;
    }
    case 'listed': {
      // Created, forked, or unarchived: the row belongs in the active
      // sidebar now. Insert it if this client does not have it — the
      // initiating client's own prepend is the same step, so an echo of
      // its own creation is idempotent.
      if (!evt.thread?.id) return;
      const merged = mergeThreadRowWithLocal(evt.thread);
      if (!getThreadById(merged.id)) prependThread(merged);
      syncThread(merged);
      return;
    }
    default: {
      // 'full': the row's current state. Says nothing about membership,
      // so a row this client does not have is not invented here — the
      // authoritative ListThreads resync owns that.
      if (evt.thread?.id) syncThreadRow(evt.thread);
    }
  }
}

export function applyModeChanged(payload: ModeChangedPayload): void {
  if (!payload || !payload.threadId) return;
  patchThreadEverywhere(payload.threadId, { mode: payload.mode });
  if (payload.needsReconnect) {
    addToast(
      'warning',
      `Mode set to ${payload.mode}. Reconnect the session to apply.`,
    );
  }
}

export function applyRuntimeModeChanged(payload: RuntimeModeChangedPayload): void {
  if (!payload || !payload.threadId || !payload.runtimeMode) return;
  patchThreadEverywhere(payload.threadId, { runtimeMode: payload.runtimeMode });
}
