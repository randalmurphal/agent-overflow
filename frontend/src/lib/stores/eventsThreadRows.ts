// Thread-row projection domain: syncing cached Thread rows (sidebar list +
// per-pane copies) against wire updates — read markers, latest-turn
// completion timestamps, activity bumps, durable proposed-plan /
// incomplete-turn status, and the thread:updated / mode-change channels.
// This is the shared leaf other events* modules import from; it never
// imports another events* module. Fan-in target of events.ts's
// setupEventListeners.
import type { TurnCompletedEvent } from '../types/events';
import type { Item, Thread } from '../types/models';
import { findPaneShowingThread, iterPanes, syncThread } from './panes.svelte';
import { refreshProjects, touchProjectActivity } from './projects.svelte';
import { addToast } from './toast.svelte';
import { getThreadById, getThreads, refreshThreads, replaceThread, touchThreadActivity } from './threads.svelte';
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

export function syncThreadRow(updated: Thread): Thread {
  const readMarkers = [updated.lastReadAt];
  const latestCompletions = [updated.latestTurnCompletedAt];
  const activityMarkers = [updated.updatedAt];
  const cachedThread = getThreadById(updated.id);
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
  const merged = { ...updated, updatedAt, lastReadAt, latestTurnCompletedAt };
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

export function refreshSidebarProjections(): void {
  void refreshThreads();
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

export function updateThreadUsageCache(threadId: string, raw: string): void {
  const existing = getThreads().find((thread) => thread.id === threadId);
  if (existing) {
    replaceThread({ ...existing, lastTokenUsage: raw });
  }
  for (const pane of iterPanes()) {
    if (pane.threadId !== threadId || !pane.thread) continue;
    pane.replaceThread({ ...pane.thread, lastTokenUsage: raw });
  }
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
  } catch {
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
  const existing = getThreads().find((t) => t.id === payload.threadId);
  if (existing) {
    replaceThread({ ...existing, mode: payload.mode });
  }
  for (const pane of iterPanes()) {
    if (pane.threadId !== payload.threadId) continue;
    if (pane.thread) {
      pane.replaceThread({ ...pane.thread, mode: payload.mode });
    }
  }
  if (payload.needsReconnect) {
    addToast(
      'warning',
      `Mode set to ${payload.mode}. Reconnect the session to apply.`,
    );
  }
}

export function applyRuntimeModeChanged(payload: RuntimeModeChangedPayload): void {
  if (!payload || !payload.threadId || !payload.runtimeMode) return;
  const existing = getThreads().find((t) => t.id === payload.threadId);
  if (existing) {
    replaceThread({ ...existing, runtimeMode: payload.runtimeMode });
  }
  for (const pane of iterPanes()) {
    if (pane.threadId !== payload.threadId) continue;
    if (pane.thread) {
      pane.replaceThread({ ...pane.thread, runtimeMode: payload.runtimeMode });
    }
  }
}
