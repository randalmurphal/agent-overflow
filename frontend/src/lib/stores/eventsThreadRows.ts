// Thread-row projection domain: syncing cached Thread rows (sidebar list +
// per-pane copies) against wire updates — read markers, latest-turn
// completion timestamps, activity bumps, durable proposed-plan /
// incomplete-turn status, and the thread:updated / mode-change channels.
// This is the shared leaf other events* modules import from; it never
// imports another events* module. Fan-in target of events.ts's
// setupEventListeners.
import type { TurnCompletedEvent } from '../types/events';
import type { Item, Thread } from '../types/models';
import { ListThreads } from './bindings';
import { findPaneShowingThread, iterPanes, syncThread } from './panes.svelte';
import { refreshProjects, touchProjectActivity } from './projects.svelte';
import { addToast } from './toast.svelte';
import { getThreadById, getThreads, replaceAllThreads, replaceThread, touchThreadActivity } from './threads.svelte';
import { parseJsonObject } from '../utils/parseJsonObject';

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
  const readMarkers = [updated.lastReadAt];
  const latestCompletions = [updated.latestTurnCompletedAt];
  const activityMarkers = [updated.updatedAt];
  if (cachedThread?.lastReadAt !== undefined) {
    readMarkers.push(cachedThread.lastReadAt);
  }
  if (cachedThread?.latestTurnCompletedAt !== undefined) {
    latestCompletions.push(cachedThread.latestTurnCompletedAt);
  }
  if (cachedThread && Number.isFinite(cachedThread.updatedAt)) {
    activityMarkers.push(cachedThread.updatedAt);
  }

  for (const pane of iterPanes()) {
    if (pane.threadId !== updated.id || !pane.thread) continue;
    if (pane.thread.lastReadAt !== undefined) {
      readMarkers.push(pane.thread.lastReadAt);
    }
    if (pane.thread.latestTurnCompletedAt !== undefined) {
      latestCompletions.push(pane.thread.latestTurnCompletedAt);
    }
    if (Number.isFinite(pane.thread.updatedAt)) {
      activityMarkers.push(pane.thread.updatedAt);
    }
  }

  const lastReadAt = mergeReadMarkersPreservingUnread(readMarkers);
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
    ?? findPaneShowingThread(evt.threadId)?.thread;
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
  const thread = touchThreadActivity(threadId, updatedAt);
  let projectId = thread?.projectId;
  let latestUpdatedAt = thread?.updatedAt ?? updatedAt;

  for (const pane of iterPanes()) {
    if (pane.threadId !== threadId || !pane.thread) continue;
    projectId = projectId ?? pane.thread.projectId;
    const paneUpdatedAt = Math.max(pane.thread.updatedAt ?? 0, updatedAt);
    latestUpdatedAt = Math.max(latestUpdatedAt, paneUpdatedAt);
    if (pane.thread.updatedAt === paneUpdatedAt) continue;
    pane.replaceThread({ ...pane.thread, updatedAt: paneUpdatedAt });
  }

  touchProjectActivity(projectId, latestUpdatedAt);
}

export function userTextCountsAsActivity(item: Item): boolean {
  if (item.kind !== 'user_text') return false;
  if (item.parentId) return false;
  const meta = parseJsonObject(item.meta);
  if (meta?.wire_only === true) return false;
  return true;
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
    rows = await ListThreads() as Thread[];
  } catch (err) {
    console.error('Failed to resync threads after transport gap:', err);
    addToast('error', 'Failed to load threads');
    return;
  }
  const cachedById = new Map(getThreads().map((thread) => [thread.id, thread]));
  const merged = rows.map((row) => mergeThreadRowWithLocal(row, cachedById.get(row.id)));
  replaceAllThreads(merged);
  const mergedById = new Map(merged.map((thread) => [thread.id, thread]));
  for (const pane of iterPanes()) {
    if (!pane.threadId || !pane.thread) continue;
    const row = mergedById.get(pane.threadId);
    if (row) pane.replaceThread(row);
  }
}

export function refreshSidebarProjections(): void {
  void resyncThreadRows();
  void refreshProjects();
}

function mergeReadMarkersPreservingUnread(readMarkers: Array<number | undefined>): number | undefined {
  const definedReadMarkers = readMarkers.filter((value): value is number => value !== undefined);
  if (definedReadMarkers.length === 0) {
    return undefined;
  }
  if (definedReadMarkers.includes(0)) {
    return 0;
  }
  return Math.max(...definedReadMarkers);
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
  for (const pane of iterPanes()) {
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
  // change the thread. This is the cooperating half of the item-upsert
  // dedupe in `applyItemUpsertsToWindow` — `syncProposedPlanStatus` fires
  // this on every proposed-plan upsert, and without the dedupe a repeated
  // upsert that doesn't move the durable status STILL replaces
  // `pane.thread` with a new reference, triggering the same reactive
  // cascade through any component that reads `pane.thread` directly.
  const existing = getThreads().find((thread) => thread.id === threadId);
  if (existing && !patchMatchesThread(existing, patch)) {
    replaceThread({ ...existing, ...patch });
  }
  for (const pane of iterPanes()) {
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

function isImplementedProposedPlan(item: Item): boolean {
  if (item.payloadKind !== 'proposed_plan' || item.role !== 'assistant' || item.status !== 'completed') {
    return false;
  }
  if (!item.meta) return false;
  try {
    const parsed = JSON.parse(item.meta) as { planImplementedAt?: number };
    return Number(parsed.planImplementedAt ?? 0) > 0;
  } catch (err) {
    // Treat unparseable meta as not-implemented (the plan stays
    // actionable), but don't lose the signal that a proposed_plan row
    // carried malformed JSON.
    console.warn(
      `isImplementedProposedPlan: unparseable proposed_plan meta for thread ${item.threadId}, item ${item.id}:`,
      err,
    );
    return false;
  }
}

export function syncProposedPlanStatus(item: Item): void {
  if (item.payloadKind !== 'proposed_plan' || item.role !== 'assistant' || item.status !== 'completed') {
    return;
  }
  patchThreadDurableStatus(item.threadId, {
    hasActionableProposedPlan: !isImplementedProposedPlan(item),
  });
}

export interface ThreadUpdateEvent {
  action: string;
  thread?: Thread;
  id?: string;
  title?: string;
  model?: string;
}

export function applyThreadUpdated(evt: ThreadUpdateEvent): void {
  if (!evt) return;
  if (evt.action === 'patch' && evt.id) {
    const cached = getThreadById(evt.id)
      ?? findPaneShowingThread(evt.id)?.thread;
    if (!cached) return;
    const merged = { ...cached };
    if (evt.title !== undefined) merged.title = evt.title;
    if (evt.model !== undefined) merged.model = evt.model;
    syncThreadRow(merged);
    return;
  }
  if (evt.thread?.id) {
    syncThreadRow(evt.thread);
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
