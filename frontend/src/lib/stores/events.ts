import { Events } from '@wailsio/runtime';
import type {
  ApprovalEvent,
  ItemDeltaEvent,
  ItemStreamEvent,
  TodoUpdateEvent,
  ProviderStatusEvent,
  SessionDiedEvent,
  SubagentNotificationEvent,
  TurnCompletedEvent,
  TurnStartedEvent,
  UsageEvent,
  UserInputEvent,
} from '../types/events';
import type { Item, Thread } from '../types/models';
import type {
  CheckpointCapturedEvent,
  CheckpointErrorEvent,
  CheckpointRevertedEvent,
  CheckpointUnavailableEvent,
} from '../types/checkpoint';
import { transportGapChannel } from '../transport/wsClient';
import {
  confirmFlushedByUserItemId,
  markItemsFlushed,
  replaceQueueForThread,
  type QueueItem as SendQueueItem,
} from './sendQueue.svelte';
import { getAllPanes, syncThread } from './panes.svelte';
import { recordProviderStatus } from './providerStatus.svelte';
import { addToast } from './toast.svelte';
import { getThreadById, getThreads, replaceThread } from './threads.svelte';
import { parseJsonObject } from '../utils/parseJsonObject';
import {
  projectApprovalRequest,
  projectApprovalResolution,
  projectThreadItem,
  projectTurnCompleted,
  projectTurnStarted,
  projectUserInputRequest,
  projectUserInputResolution,
} from './threadStatuses.svelte';
import { parseTokenUsage } from './thread.svelte';

/**
 * Min interval between consecutive `design:reload-main` cache-bust
 * fires per thread. Watcher events on a hot save loop can land in
 * tight bursts; throttling keeps the iframe from re-creating its
 * document tree more than twice a second.
 */
const DESIGN_RELOAD_THROTTLE_MS = 500;
const designReloadLastFireAt: Map<string, number> = new Map();
const designReloadPending: Map<string, ReturnType<typeof setTimeout>> = new Map();

interface DesignReloadMainPayload {
  threadId: string;
}
interface DesignOptionsUpdatePayload {
  threadId: string;
  setId: string;
}
/**
 * Frontend event names for design-mode UI handlers. The preview panel
 * subscribes to these (not Wails events) so the throttled handler
 * below stays the single fan-out point: each Wails event makes at
 * most one DOM event per thread per throttle window.
 */
export const DESIGN_RELOAD_MAIN_EVENT = 'ao-design:reload-main';
export const DESIGN_OPTIONS_UPDATE_EVENT = 'ao-design:options-update';

function dispatchDomEvent(name: string, detail: unknown): void {
  if (typeof window === 'undefined') return;
  window.dispatchEvent(new CustomEvent(name, { detail }));
}

function fireReloadMain(threadId: string): void {
  designReloadLastFireAt.set(threadId, Date.now());
  dispatchDomEvent(DESIGN_RELOAD_MAIN_EVENT, { threadId });
}

function handleDesignReloadMain(payload: DesignReloadMainPayload | undefined): void {
  if (!payload?.threadId) return;
  const threadId = payload.threadId;
  const lastFire = designReloadLastFireAt.get(threadId) ?? 0;
  const elapsed = Date.now() - lastFire;
  if (elapsed >= DESIGN_RELOAD_THROTTLE_MS) {
    const pending = designReloadPending.get(threadId);
    if (pending !== undefined) {
      clearTimeout(pending);
      designReloadPending.delete(threadId);
    }
    fireReloadMain(threadId);
    return;
  }
  // A fire is already pending — coalesce with it.
  if (designReloadPending.has(threadId)) return;
  const delay = DESIGN_RELOAD_THROTTLE_MS - elapsed;
  const handle = setTimeout(() => {
    designReloadPending.delete(threadId);
    fireReloadMain(threadId);
  }, delay);
  designReloadPending.set(threadId, handle);
}

// Same throttle pattern, applied to options-update. Without throttling
// the options panel re-fetches once per file written into options/.
const DESIGN_OTHER_THROTTLE_MS = 250;
type DesignThrottleMaps = {
  lastFire: Map<string, number>;
  pending: Map<string, ReturnType<typeof setTimeout>>;
};
const designOptionsThrottle: DesignThrottleMaps = {
  lastFire: new Map(),
  pending: new Map(),
};

function fireThrottled(
  state: DesignThrottleMaps,
  threadId: string,
  intervalMs: number,
  fire: () => void,
): void {
  const lastFire = state.lastFire.get(threadId) ?? 0;
  const elapsed = Date.now() - lastFire;
  if (elapsed >= intervalMs) {
    const pending = state.pending.get(threadId);
    if (pending !== undefined) {
      clearTimeout(pending);
      state.pending.delete(threadId);
    }
    state.lastFire.set(threadId, Date.now());
    fire();
    return;
  }
  if (state.pending.has(threadId)) return;
  const delay = intervalMs - elapsed;
  const handle = setTimeout(() => {
    state.pending.delete(threadId);
    state.lastFire.set(threadId, Date.now());
    fire();
  }, delay);
  state.pending.set(threadId, handle);
}

function clearDesignThrottle(state: DesignThrottleMaps): void {
  for (const handle of state.pending.values()) {
    clearTimeout(handle);
  }
  state.pending.clear();
  state.lastFire.clear();
}

const itemUpsertSubscribers: Set<(item: Item) => void> = new Set();
const ITEM_EVENT_FLUSH_MAX_DELAY_MS = 50;
const ITEM_EVENT_FLUSH_MAX_EVENTS = 500;
const ITEM_EVENT_QUEUE_FORCE_FLUSH_EVENTS = 2_000;
const ITEM_EVENT_TEXT_FIELD_MAX_CHARS = 2_000_000;
let itemEventQueue: ItemStreamEvent[] = [];
let itemEventQueueStart = 0;
let itemEventFlushFrame: number | null = null;
let itemEventFlushTimeout: number | null = null;

function requestFrame(callback: () => void): number {
  if (typeof requestAnimationFrame === 'function') {
    return requestAnimationFrame(callback);
  }
  return window.setTimeout(callback, 0);
}

function cancelFrame(handle: number): void {
  if (typeof cancelAnimationFrame === 'function') {
    cancelAnimationFrame(handle);
  } else {
    window.clearTimeout(handle);
  }
}

function cancelItemEventFlushSchedule(): void {
  if (itemEventFlushFrame !== null) {
    cancelFrame(itemEventFlushFrame);
    itemEventFlushFrame = null;
  }
  if (itemEventFlushTimeout !== null) {
    window.clearTimeout(itemEventFlushTimeout);
    itemEventFlushTimeout = null;
  }
}

function scheduleItemEventFlush(): void {
  if (itemEventFlushFrame !== null || itemEventFlushTimeout !== null) return;
  itemEventFlushFrame = requestFrame(flushItemEventQueue);
  itemEventFlushTimeout = window.setTimeout(flushItemEventQueue, ITEM_EVENT_FLUSH_MAX_DELAY_MS);
}

function resetItemEventQueue(): void {
  cancelItemEventFlushSchedule();
  itemEventQueue = [];
  itemEventQueueStart = 0;
}

function isFiniteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

function isBoundedString(value: unknown, maxChars = ITEM_EVENT_TEXT_FIELD_MAX_CHARS): value is string {
  return typeof value === 'string' && value.length <= maxChars;
}

function isValidItemForThread(item: Item | null | undefined, threadId: string): item is Item {
  if (!item || item.threadId !== threadId) return false;
  if (!isBoundedString(item.id, 512) || item.id.trim() === '') return false;
  if (!isBoundedString(item.threadId, 512) || item.threadId.trim() === '') return false;
  if (!isFiniteNumber(item.turnIndex) || !isFiniteNumber(item.itemIndex)) return false;
  if (!isBoundedString(item.kind, 128)) return false;
  if (!isBoundedString(item.role, 128)) return false;
  if (!isBoundedString(item.status, 128)) return false;
  if (!isBoundedString(item.summary)) return false;
  if (item.payloadId !== undefined && !isBoundedString(item.payloadId, 512)) return false;
  if (item.payloadKind !== undefined && !isBoundedString(item.payloadKind, 128)) return false;
  if (item.payloadMeta !== undefined && !isBoundedString(item.payloadMeta)) return false;
  if (item.parentId !== undefined && !isBoundedString(item.parentId, 512)) return false;
  if (item.completionOf !== undefined && !isBoundedString(item.completionOf, 512)) return false;
  if (item.toolName !== undefined && !isBoundedString(item.toolName, 256)) return false;
  if (item.meta !== undefined && !isBoundedString(item.meta)) return false;
  if (!isFiniteNumber(item.createdAt) || !isFiniteNumber(item.updatedAt)) return false;
  return true;
}

export function onItemUpsert(handler: (item: Item) => void): () => void {
  itemUpsertSubscribers.add(handler);
  return () => {
    itemUpsertSubscribers.delete(handler);
  };
}

function notifyItemUpserts(items: Item[]): void {
  if (items.length === 0 || itemUpsertSubscribers.size === 0) return;
  const subscribers = [...itemUpsertSubscribers];
  for (const item of items) {
    for (const handler of subscribers) {
      handler(item);
    }
  }
}

/**
 * wailsEventOn wraps Events.On so callers receive the inner Go payload.
 * The transport (wsClient.ts) is payload-agnostic and Phase C made the
 * production wire deliver raw payloads, so this helper is just an
 * import-path shim — the wsClient already hands `ev.data` through as
 * the bare payload.
 *
 * Per-channel gap detection lives in the transport: the wsClient
 * surfaces gaps via the synthetic `transport:gap` channel and the
 * `gap:true` flag on `event` frames. Subscribers that care about gap
 * recovery should consume that channel directly rather than
 * re-implementing seq tracking here.
 *
 * Exported so subscribers outside this file (terminal drawer, diff
 * panel) keep a single import path.
 */
export function wailsEventOn<T = unknown>(
  name: string,
  handler: (data: T) => void,
): () => void {
  return Events.On(name, (ev) => handler(ev.data as T));
}

/**
 * Payload for the backend-emitted thread:mode_changed event. Mirrors
 * ThreadModeChangedEvent in app_thread_interaction_mode.go.
 */
interface ModeChangedPayload {
  threadId: string;
  mode: NonNullable<Thread['mode']>;
  needsReconnect: boolean;
}

/**
 * Payload for thread:runtime_mode_changed — emitted whenever
 * SetThreadRuntimeMode persists a change. Runtime-mode changes restart
 * active sessions synchronously, so needsReconnect is false on success and
 * kept only for compatibility with the older event shape.
 */
interface RuntimeModeChangedPayload {
  threadId: string;
  runtimeMode: Thread['runtimeMode'];
  needsReconnect: boolean;
}

function syncThreadRow(updated: Thread): void {
  const readMarkers = [updated.lastReadAt];
  const latestCompletions = [updated.latestTurnCompletedAt];
  const cachedThread = getThreadById(updated.id);
  if (cachedThread?.lastReadAt !== undefined) {
    readMarkers.push(cachedThread.lastReadAt);
  }
  if (cachedThread?.latestTurnCompletedAt !== undefined) {
    latestCompletions.push(cachedThread.latestTurnCompletedAt);
  }

  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== updated.id || !pane.thread) continue;
    if (pane.thread.lastReadAt !== undefined) {
      readMarkers.push(pane.thread.lastReadAt);
    }
    if (pane.thread.latestTurnCompletedAt !== undefined) {
      latestCompletions.push(pane.thread.latestTurnCompletedAt);
    }
  }

  const lastReadAt = mergeReadMarkersPreservingUnread(readMarkers);
  const latestTurnCompletedAt = mergeLatestTurnCompletedAt(latestCompletions);
  syncThread({ ...updated, lastReadAt, latestTurnCompletedAt });
}

function syncLatestTurnCompleted(evt: TurnCompletedEvent): void {
  const cachedThread = getThreadById(evt.threadId)
    ?? [...getAllPanes().values()].find((pane) => pane.threadId === evt.threadId)?.thread;
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

function updateThreadUsageCache(threadId: string, raw: string): void {
  const existing = getThreads().find((thread) => thread.id === threadId);
  if (existing) {
    replaceThread({ ...existing, lastTokenUsage: raw });
  }
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== threadId || !pane.thread) continue;
    pane.replaceThread({ ...pane.thread, lastTokenUsage: raw });
  }
}

function patchThreadDurableStatus(
  threadId: string,
  patch: Pick<Partial<Thread>, 'hasActionableProposedPlan' | 'hasIncompleteTurn'>,
): void {
  const existing = getThreads().find((thread) => thread.id === threadId);
  if (existing) {
    replaceThread({ ...existing, ...patch });
  }
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== threadId || !pane.thread) continue;
    pane.replaceThread({ ...pane.thread, ...patch });
  }
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

function syncProposedPlanStatus(item: Item): void {
  if (item.payloadKind !== 'proposed_plan' || item.role !== 'assistant' || item.status !== 'completed') {
    return;
  }
  patchThreadDurableStatus(item.threadId, {
    hasActionableProposedPlan: !isImplementedProposedPlan(item),
  });
}

function applyApprovalEvent(evt: ApprovalEvent): void {
  if (!evt) return;

  if (evt.action === 'request' && evt.request?.threadId) {
    projectApprovalRequest(
      evt.request.threadId,
      evt.request.requestId,
      evt.request.kind,
    );
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === evt.request.threadId) {
        pane.addApproval(evt.request);
      }
    }
    return;
  }

  if ((evt.action === 'resolve' || evt.action === 'fail') && evt.requestId) {
    projectApprovalResolution(evt.threadId, evt.requestId);
    for (const pane of getAllPanes().values()) {
      if (evt.threadId && pane.threadId !== evt.threadId) continue;
      const hadApproval = pane.pendingApprovals.some((approval) => approval.requestId === evt.requestId);
      pane.removeApproval(evt.requestId);
      if (hadApproval && evt.action === 'fail' && evt.detail) {
        pane.setGeneralError(`Failed to respond to approval: ${evt.detail}`);
      }
    }
  }
}

function applyUserInputEvent(evt: UserInputEvent): void {
  if (!evt) return;

  if (evt.action === 'request' && evt.request?.threadId) {
    projectUserInputRequest(evt.request.threadId, evt.request.requestId);
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === evt.request.threadId) {
        pane.addUserInput(evt.request);
      }
    }
    return;
  }

  if ((evt.action === 'resolve' || evt.action === 'fail') && evt.requestId) {
    projectUserInputResolution(evt.threadId, evt.requestId);
    for (const pane of getAllPanes().values()) {
      if (evt.threadId && pane.threadId !== evt.threadId) continue;
      const hadRequest = pane.pendingUserInputs.some((request) => request.requestId === evt.requestId);
      pane.removeUserInput(evt.requestId);
      if (hadRequest && evt.action === 'fail' && evt.detail) {
        pane.setGeneralError(`Failed to submit input: ${evt.detail}`);
      }
    }
  }
}

function applyUsageEvent(evt: UsageEvent): void {
  if (!evt?.threadId) return;

  // `rate_limits` piggybacks on the same channel but doesn't touch the
  // context-window ring. Bail before the ring-update path so a rate-limit
  // refresh never clobbers the last known token-window snapshot.
  if (evt.action === 'rate_limits') {
    // Future work: thread this onto a pane-level rateLimits state so the
    // popover can render the snapshot. For v1 the backend just keeps
    // capturing it; no pane surface yet. Explicitly returning here makes
    // the "no-op" intentional and greppable.
    return;
  }

  const payload = evt.action === 'usage'
    ? {
        usedTokens: evt.usedTokens ?? 0,
        maxTokens: evt.maxTokens,
        usedPercentage: evt.contextPercent,
        ...(evt.autoCompactPercent ? { autoCompactPercent: evt.autoCompactPercent } : {}),
        ...(evt.autoCompactTokenLimit ? { autoCompactTokenLimit: evt.autoCompactTokenLimit } : {}),
      }
    : null;

  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== evt.threadId) continue;
    if (payload) {
      pane.setContextWindow(payload);
    } else {
      pane.clearContextWindow();
    }
  }

  updateThreadUsageCache(
    evt.threadId,
    payload
      ? JSON.stringify({
          usedTokens: payload.usedTokens,
          maxTokens: payload.maxTokens,
          contextPercent: payload.usedPercentage,
          autoCompactPercent: payload.autoCompactPercent,
          autoCompactTokenLimit: payload.autoCompactTokenLimit,
        })
      : '',
  );
}

function applyItemUpserts(items: Item[]): void {
  if (items.length === 0) return;
  const itemsByThread = new Map<string, Item[]>();
  for (const item of items) {
    const list = itemsByThread.get(item.threadId);
    if (list) {
      list.push(item);
    } else {
      itemsByThread.set(item.threadId, [item]);
    }
    // Zone 2 clears only when the provider-confirmed user_text row
    // arrives. Normal queued sends no longer create an optimistic chat
    // row at flush time; the row's provider_item_id is the signal that
    // Claude/Codex accepted it into context.
    if (item.kind === 'user_text' && item.id.includes(':flush:') && itemHasProviderItemID(item)) {
      confirmFlushedByUserItemId(item.threadId, item.id);
    }
  }
  for (const pane of getAllPanes().values()) {
    const threadItems = pane.threadId ? itemsByThread.get(pane.threadId) : undefined;
    if (!threadItems) continue;
    pane.upsertItems(threadItems);
  }
}

function itemHasProviderItemID(item: Item): boolean {
  const parsed = parseJsonObject(item.meta);
  const id = parsed?.provider_item_id ?? parsed?.providerItemId;
  return typeof id === 'string' && id.trim().length > 0;
}

function applyItemDelta(evt: ItemDeltaEvent): void {
  if (!evt || !evt.threadId || !evt.itemId || !evt.delta) return;
  if (!isBoundedString(evt.threadId, 512) || !isBoundedString(evt.itemId, 512)) return;
  if (!isBoundedString(evt.kind, 128) || !isBoundedString(evt.delta)) return;
  if (!isFiniteNumber(evt.updatedAt)) return;

  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.applyItemDelta(evt);
  }
}

function applyItemStreamEvent(evt: ItemStreamEvent): void {
  if (!evt || !evt.threadId) return;
  if (evt.action === 'upsert' && evt.item) {
    if (!isValidItemForThread(evt.item, evt.threadId)) return;
    projectThreadItem(evt.item);
    syncProposedPlanStatus(evt.item);
  } else if (evt.action === 'delta') {
    if (!isBoundedString(evt.threadId, 512)) return;
    if (!isBoundedString(evt.itemId, 512) || evt.itemId.trim() === '') return;
    if (!isBoundedString(evt.kind, 128)) return;
    if (!isBoundedString(evt.delta) || evt.delta === '') return;
    if (!isFiniteNumber(evt.updatedAt)) return;
  } else {
    return;
  }
  if (itemEventQueue.length - itemEventQueueStart >= ITEM_EVENT_QUEUE_FORCE_FLUSH_EVENTS) {
    flushItemEventQueue();
  }
  itemEventQueue.push(evt);
  scheduleItemEventFlush();
}

function flushItemEventQueue(): void {
  cancelItemEventFlushSchedule();
  if (itemEventQueueStart >= itemEventQueue.length) {
    itemEventQueue = [];
    itemEventQueueStart = 0;
    return;
  }

  const itemEventQueueEnd = Math.min(
    itemEventQueueStart + ITEM_EVENT_FLUSH_MAX_EVENTS,
    itemEventQueue.length,
  );
  const events = itemEventQueue.slice(itemEventQueueStart, itemEventQueueEnd);
  if (itemEventQueueEnd >= itemEventQueue.length) {
    itemEventQueue = [];
    itemEventQueueStart = 0;
  } else {
    itemEventQueueStart = itemEventQueueEnd;
    if (itemEventQueueStart > ITEM_EVENT_FLUSH_MAX_EVENTS * 4) {
      itemEventQueue = itemEventQueue.slice(itemEventQueueStart);
      itemEventQueueStart = 0;
    }
  }
  const pendingUpserts: Item[] = [];
  const notifiedUpserts: Item[] = [];
  const pendingDeltas = new Map<string, ItemDeltaEvent & { chunks: string[] }>();
  const deltaThreadIds = new Set<string>();

  const flushPendingUpserts = () => {
    if (pendingUpserts.length === 0) return;
    applyItemUpserts(pendingUpserts);
    notifiedUpserts.push(...pendingUpserts);
    pendingUpserts.length = 0;
  };

  const queueDelta = (evt: ItemDeltaEvent) => {
    const key = `${evt.threadId}\u0000${evt.itemId}\u0000${evt.kind}`;
    const existing = pendingDeltas.get(key);
    if (existing) {
      existing.chunks.push(evt.delta);
      existing.updatedAt = Math.max(existing.updatedAt, evt.updatedAt);
      return;
    }
    pendingDeltas.set(key, { ...evt, delta: '', chunks: [evt.delta] });
  };

  const flushPendingDeltas = () => {
    if (pendingDeltas.size === 0) return;
    for (const delta of pendingDeltas.values()) {
      const coalesced: ItemDeltaEvent = {
        threadId: delta.threadId,
        itemId: delta.itemId,
        kind: delta.kind,
        delta: delta.chunks.join(''),
        updatedAt: delta.updatedAt,
      };
      applyItemDelta(coalesced);
      deltaThreadIds.add(coalesced.threadId);
    }
    pendingDeltas.clear();
  };

  for (const evt of events) {
    if (!evt || !evt.threadId) continue;
    if (evt.action === 'upsert') {
      flushPendingDeltas();
      if (!isValidItemForThread(evt.item, evt.threadId)) continue;
      pendingUpserts.push(evt.item);
      continue;
    }
    if (evt.action !== 'delta') continue;

    flushPendingUpserts();
    queueDelta(evt);
  }

  flushPendingDeltas();
  flushPendingUpserts();
  if (deltaThreadIds.size > 0) {
    for (const pane of getAllPanes().values()) {
      const threadId = pane.threadId;
      if (threadId && deltaThreadIds.has(threadId)) {
        pane.flushLiveDeltas();
      }
    }
  }
  notifyItemUpserts(notifiedUpserts);
  if (itemEventQueueStart < itemEventQueue.length) {
    scheduleItemEventFlush();
  }
}

// kindToLegacyStatus maps the chat-rewrite closed kind enum onto the legacy
// `status` vocabulary the ProviderStatusBanner already renders. Keeps the
// banner component untouched while the router adopts the new vocabulary —
// the two pipelines converge here rather than in the view.
//
// Retry vocabulary lives on `provider:item_event` (`api_retry` row) now,
// not on this banner channel; session-death drives `pane.generalError`
// via `provider:session_died`. So the legacy mapping only needs to cover
// the boot-time provider-presence states.
const KIND_TO_LEGACY_STATUS: Record<NonNullable<ProviderStatusEvent['kind']>, ProviderStatusEvent['status']> = {
  binary_missing: 'not_found',
  unauthenticated: 'unauthenticated',
  version_incompatible: 'version_too_old',
};

function applyProviderStatus(evt: ProviderStatusEvent): void {
  if (!evt) return;

  // Chat-rewrite emissions carry `kind` and optionally `threadId`. The
  // legacy binary-detect emissions carry `provider + status`. Derive a
  // unified shape before fanning out so downstream consumers don't have
  // to branch.
  let effectiveStatus = evt.status;
  if (evt.kind) {
    const mapped = KIND_TO_LEGACY_STATUS[evt.kind];
    if (!mapped) {
      // An unknown kind leaks the banner to the console so the gap is
      // visible in dev — the spec calls this out as "require updating the
      // frontend banner component in the same PR". Drop without rendering.
      console.warn(`provider:status: unknown kind "${evt.kind}" — dropped`);
      return;
    }
    effectiveStatus = mapped;
  }

  if (!evt.provider || !effectiveStatus) return;

  const normalized: ProviderStatusEvent = { ...evt, status: effectiveStatus };

  // Single source of truth: update the app-wide per-provider map first
  // so any consumer reading via getProviderStatus sees the latest
  // snapshot regardless of whether the event arrived with a `status`
  // (legacy) or a `kind` (chat-rewrite). The map is mutated with the
  // normalized event so legacy banner consumers stay untouched.
  recordProviderStatus(normalized);

  const banner = effectiveStatus === 'ready' ? null : normalized;
  for (const pane of getAllPanes().values()) {
    if (pane.thread?.provider !== evt.provider) continue;
    // Kind-bearing events can carry a threadId for per-pane scoping; when
    // present, only update the matching pane. Without a threadId the event
    // is provider-global (legacy behavior) and fans out to every matching
    // pane as before.
    if (evt.threadId && pane.threadId !== evt.threadId) continue;
    pane.setProviderBanner(banner);
  }
}

function applyThreadUpdated(updated: Thread): void {
  if (!updated?.id) return;
  syncThreadRow(updated);
}

/**
 * Route `provider:turn_started` to the global active-turn registry
 * (single source of truth — see threadStatuses.svelte.ts). Both the
 * sidebar pill and the chat working indicator read from there. This
 * is the only path that can record a live turn — invariant 22 (turn
 * activity is wire-pushed, never derived from items).
 */
function applyTurnStarted(evt: TurnStartedEvent): void {
  if (!evt?.threadId || !evt.turnId) return;
  // Pass the full {turnIndex, startedAt} into the global registry so
  // the chat working indicator's self-ticking timer and the timeline
  // boundary projection can read both without a separate write path.
  projectTurnStarted(evt.threadId, evt.turnId, evt.turnIndex, evt.startedAt);
  patchThreadDurableStatus(evt.threadId, {
    hasActionableProposedPlan: false,
    hasIncompleteTurn: false,
  });
}

/**
 * Route `provider:turn_completed` to the matching pane. Clears the
 * global active-turn registry entry (threadStatuses) and writes the
 * settled projection for read-state and trace/debug consumers.
 *
 * `tokenUsage` arrives as a JSON-encoded string on the wire because
 * triage round-trips it through the DB's `token_usage_json` column. We
 * parse it here via `parseTokenUsage` — the same helper the pane uses on
 * thread-switch rehydration — so malformed JSON degrades gracefully to
 * `tokenUsage: null` rather than crashing the listener.
 */
function applyTurnCompleted(evt: TurnCompletedEvent): void {
  if (!evt?.threadId || !evt.turnId) return;
  const rawAssistantId = evt.assistantMessageId ?? '';
  const settled = {
    turnId: evt.turnId,
    turnIndex: evt.turnIndex,
    startedAt: evt.startedAt,
    completedAt: evt.completedAt,
    stopReason: evt.stopReason ?? '',
    assistantMessageId: rawAssistantId === '' ? null : rawAssistantId,
    tokenUsage: parseTokenUsage(evt.tokenUsage),
    aborted: Boolean(evt.aborted),
    errorMessage: evt.errorMessage ?? '',
  };
  // Clear the turn from the sidebar projection. Errored turns flip the
  // pill to Failed; clean aborts flip it to Interrupted.
  projectTurnCompleted(evt.threadId, evt.turnId, {
    aborted: settled.aborted,
    errorMessage: settled.errorMessage,
  });
  patchThreadDurableStatus(evt.threadId, { hasIncompleteTurn: false });
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.settleTurn(settled);
  }
  syncLatestTurnCompleted(evt);
  // Send-queue drain is owned by the backend. Triage flushes queued
  // messages at safe provider boundaries and the frontend mirrors that
  // state via `provider:queue_state_changed` / `provider:queue_flushed`.
  // Zone 2 clears only when a matching `provider:item_event` upsert
  // carries `provider_item_id`, proving the provider echo arrived.
}

/**
 * Route `provider:session_died` to the matching pane's banner slot.
 * The wire-side row in the timeline (kind `notification` with
 * `meta.kind = "session_died"`) provides the historical trace; this
 * listener flips `pane.generalError` so the existing
 * `ProviderStatusBanner` Reconnect-button banner fires. The triage
 * router synthesizes the truncated `provider:turn_completed` on its
 * own — that path clears the working indicator independently, so this
 * listener never has to touch turn state.
 */
function applySessionDied(evt: SessionDiedEvent): void {
  if (!evt?.threadId) return;
  const message = sessionDiedBannerMessage(evt);
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.setGeneralError(message);
  }
}

function sessionDiedBannerMessage(evt: SessionDiedEvent): string {
  const reason = (evt.reason ?? '').trim();
  const signal = (evt.signal ?? '').trim();
  if (reason) return reason;
  if (signal) return `Provider session terminated by signal ${signal}`;
  if (evt.exitCode) return `Provider session exited with code ${evt.exitCode}`;
  return 'Provider session exited unexpectedly';
}

/**
 * Route `provider:subagent_notification` to the matching pane. No UI
 * consumes this today; the pane records it in a bounded log so a future
 * tray / toast surface can subscribe without re-wiring the channel.
 */
function applySubagentNotification(evt: SubagentNotificationEvent): void {
  if (!evt?.threadId) return;
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.appendSubagentNotification(evt);
  }
}

/**
 * Route `provider:todo_update` to the matching pane. Updates the
 * Todos segment of the activity rail. Empty step arrays clear the
 * snapshot; an all-completed snapshot starts the auto-hide timer
 * inside `setLiveTodo`. Todo updates do NOT add a timeline row — the
 * snapshot lives only in pane state.
 */
function applyTodoUpdate(evt: TodoUpdateEvent): void {
  if (!evt?.threadId) return;
  const steps = Array.isArray(evt.steps) ? evt.steps : [];
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.setLiveTodo(steps);
  }
}

/**
 * Set up the app's Wails event listeners.
 * Returns a cleanup function that removes all listeners.
 */
export function setupEventListeners(): () => void {
  resetItemEventQueue();

  const cancelApproval = wailsEventOn<ApprovalEvent>('provider:approval', applyApprovalEvent);
  const cancelUserInput = wailsEventOn<UserInputEvent>('provider:user_input', applyUserInputEvent);

  const cancelUsage = wailsEventOn<UsageEvent>('provider:usage', applyUsageEvent);

  const cancelProviderStatus = wailsEventOn<ProviderStatusEvent>('provider:status', applyProviderStatus);

  // provider:item_event is the canonical ordered timeline mutation stream.
  // Upserts and deltas intentionally share one Wails channel so streaming
  // text cannot race lifecycle snapshots across separate event names.
  const cancelItemEvent = wailsEventOn<ItemStreamEvent>('provider:item_event', applyItemStreamEvent);

  // provider:turn_{started,completed} — wire-pushed turn lifecycle.
  // These are the sole drivers of the global active-turn registry
  // (threadStatuses.svelte.ts → getActiveTurn) and
  // `pane.latestSettledTurn`. See invariant 22 and
  // docs/architecture/turn-lifecycle.md §Frontend state shape.
  const cancelTurnStarted = wailsEventOn<TurnStartedEvent>('provider:turn_started', applyTurnStarted);
  const cancelTurnCompleted = wailsEventOn<TurnCompletedEvent>('provider:turn_completed', applyTurnCompleted);
  // provider:session_died — provider subprocess exited mid-turn. Drives
  // the per-pane Reconnect banner (separately from the synthesized
  // turn-completed event that clears the working indicator). The
  // historical trace lives in the timeline as a `notification` row.
  const cancelSessionDied = wailsEventOn<SessionDiedEvent>('provider:session_died', applySessionDied);
  // provider:subagent_notification — Codex passes subagent metadata
  // through; no UI renders this yet, but the pane records it so future
  // surfaces can subscribe without re-wiring.
  const cancelSubagentNotification = wailsEventOn<SubagentNotificationEvent>(
    'provider:subagent_notification',
    applySubagentNotification,
  );
  // provider:todo_update — Claude TodoWrite + Codex update_plan funnel
  // through here after parser normalisation. Drives the activity
  // rail's Todos segment. Has zero timeline footprint by design (see
  // ActivityRail.svelte).
  const cancelTodoUpdate = wailsEventOn<TodoUpdateEvent>(
    'provider:todo_update',
    applyTodoUpdate,
  );

  // provider:queue_state_changed — backend per-thread queue snapshot.
  // Authoritative replacement of the frontend's Zone 1 mirror;
  // arrives on RegisterQueueItem / UndoQueuedItems and after the
  // flush trigger drains the batch. provider:queue_flushed follows
  // successful provider writes, so failed items never enter Zone 2.
  const cancelQueueStateChanged = wailsEventOn<QueueStateChangedPayload>(
    'provider:queue_state_changed',
    applyQueueStateChanged,
  );
  const cancelQueueFlushed = wailsEventOn<QueueFlushedPayload>(
    'provider:queue_flushed',
    applyQueueFlushed,
  );

  const cancelCheckpointCaptured = wailsEventOn<CheckpointCapturedEvent | null>(
    'checkpoint:captured',
    (payload) => {
      for (const pane of getAllPanes().values()) {
        pane.applyCheckpointCaptured(payload);
      }
    },
  );
  const cancelCheckpointUnavailable = wailsEventOn<CheckpointUnavailableEvent | null>(
    'checkpoint:unavailable',
    (payload) => {
      for (const pane of getAllPanes().values()) {
        pane.applyCheckpointUnavailable(payload);
      }
    },
  );
  const cancelCheckpointError = wailsEventOn<CheckpointErrorEvent | null>(
    'checkpoint:error',
    (payload) => {
      for (const pane of getAllPanes().values()) {
        pane.applyCheckpointError(payload);
      }
    },
  );
  const cancelCheckpointReverted = wailsEventOn<CheckpointRevertedEvent | null>(
    'checkpoint:reverted',
    (payload) => {
      for (const pane of getAllPanes().values()) {
        pane.applyCheckpointReverted(payload);
      }
    },
  );

  const cancelThreadUpdated = wailsEventOn<Thread>('thread:updated', applyThreadUpdated);

  // provider:default_swapped — backend auto-flipped the default
  // provider because the saved one was not_found and the other was
  // ready. Surface a toast so the user notices the change before they
  // wonder why the next thread routed to a different CLI; the value
  // can still be reverted manually in Settings.
  interface DefaultSwappedPayload {
    from?: string;
    to?: string;
    fromCli?: string;
    otherCli?: string;
    reason?: string;
  }
  const cancelDefaultSwapped = wailsEventOn<DefaultSwappedPayload>(
    'provider:default_swapped',
    (payload) => {
      if (!payload || !payload.to) return;
      const next = payload.otherCli || payload.to;
      const prev = payload.fromCli || payload.from || 'previous default';
      addToast(
        'info',
        `Default provider switched to ${next} — ${prev} CLI not detected.`,
      );
    },
  );

  // transport:gap — synthetic event fired by wsClient.ts when the
  // server reports a missed seq on a channel. Coarse-grained recovery:
  // re-fetch the active pane's window so SQLite (the authoritative
  // history cache) backfills whatever was lost. We don't try to be
  // surgical because the gap signal doesn't carry the missed range.
  //
  // The handler matches on the channel name we lost rather than each
  // payload kind because a single gap on `provider:item_event` can
  // straddle upserts AND deltas; refreshing the whole pane is the
  // simplest correct response.
  const cancelTransportGap = wailsEventOn<{ channel: string; seq: number }>(
    transportGapChannel,
    (gap) => {
      if (!gap || typeof gap.channel !== 'string') return;
      switch (gap.channel) {
        case 'provider:item_event':
        case 'provider:turn_started':
        case 'provider:turn_completed':
        case 'thread:updated': {
          for (const pane of getAllPanes().values()) {
            if (!pane.threadId) continue;
            void pane.refreshFromBackend();
          }
          return;
        }
        default:
          // Unknown channel: log a breadcrumb and refresh active panes
          // anyway. Refreshing is cheap; missing a refresh on a future
          // channel that needs one would be silent data drift.
          console.warn(
            `events: transport gap on unknown channel "${gap.channel}" — refreshing active panes`,
          );
          for (const pane of getAllPanes().values()) {
            if (!pane.threadId) continue;
            void pane.refreshFromBackend();
          }
      }
    },
  );

  // design:reload-main — file watcher fired in the thread's main/
  // directory. The preview panel listens for the throttled DOM event we
  // re-dispatch and bumps its cache-bust counter. Throttling lives here
  // (not in the panel) so a rapid burst of saves only causes one
  // iframe reload per 500ms across all consumers.
  const cancelDesignReloadMain = wailsEventOn<DesignReloadMainPayload>(
    'design:reload-main',
    handleDesignReloadMain,
  );

  // design:options-update — agent rewrote files in options/{setId}/ for
  // the thread. Hydrates `pane.activeOptionSet` for the matching pane
  // (so the N-up grid renders) and forwards a DOM event for any future
  // component that needs the raw signal. Throttled per-thread so a
  // burst of file writes doesn't fan out a list-options RPC for each.
  const cancelDesignOptionsUpdate = wailsEventOn<DesignOptionsUpdatePayload>(
    'design:options-update',
    (payload) => {
      if (!payload?.threadId) return;
      const detail = payload;
      fireThrottled(designOptionsThrottle, payload.threadId, DESIGN_OTHER_THROTTLE_MS, () => {
        for (const pane of getAllPanes().values()) {
          if (pane.threadId === detail.threadId) {
            void pane.applyDesignOptionsUpdate(detail.threadId, detail.setId ?? '');
          }
        }
        dispatchDomEvent(DESIGN_OPTIONS_UPDATE_EVENT, detail);
      });
    },
  );

  // thread:runtime_mode_changed — backend persisted a new three-tier
  // approval mode. Refresh the sidebar cache and active pane; AccessToggle
  // only stages draft intent, so this event or SendMessageWithOptions'
  // returned Thread is what makes persisted runtime state visible.
  const cancelRuntimeModeChanged = wailsEventOn<RuntimeModeChangedPayload>(
    'thread:runtime_mode_changed',
    (payload) => {
      if (!payload || !payload.threadId || !payload.runtimeMode) return;
      const existing = getThreads().find((t) => t.id === payload.threadId);
      if (existing) {
        replaceThread({ ...existing, runtimeMode: payload.runtimeMode });
      }
      for (const pane of getAllPanes().values()) {
        if (pane.threadId !== payload.threadId) continue;
        if (pane.thread) {
          pane.replaceThread({ ...pane.thread, runtimeMode: payload.runtimeMode });
        }
      }
    },
  );

  // thread:mode_changed — the backend persisted a new mode. We update the
  // cached thread row (so sidebar badges refresh) and, when the change
  // landed on an active session, surface a toast prompting the user to
  // reconnect so the session can pick up the new mode's config.
  const cancelModeChanged = wailsEventOn<ModeChangedPayload>(
    'thread:mode_changed',
    (payload) => {
      if (!payload || !payload.threadId) return;
      const existing = getThreads().find((t) => t.id === payload.threadId);
      if (existing) {
        replaceThread({ ...existing, mode: payload.mode });
      }
      for (const pane of getAllPanes().values()) {
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
    },
  );

  return () => {
    resetItemEventQueue();
    cancelApproval();
    cancelUserInput();
    cancelUsage();
    cancelProviderStatus();
    cancelItemEvent();
    cancelTurnStarted();
    cancelTurnCompleted();
    cancelSessionDied();
    cancelSubagentNotification();
    cancelTodoUpdate();
    cancelQueueStateChanged();
    cancelQueueFlushed();
    cancelCheckpointCaptured();
    cancelCheckpointUnavailable();
    cancelCheckpointError();
    cancelCheckpointReverted();
    cancelThreadUpdated();
    cancelDefaultSwapped();
    cancelTransportGap();
    cancelDesignReloadMain();
    cancelDesignOptionsUpdate();
    cancelModeChanged();
    cancelRuntimeModeChanged();
    // Drop any pending throttled reloads + per-thread last-fire
    // bookkeeping so a re-attached listener starts from a clean state.
    for (const handle of designReloadPending.values()) {
      clearTimeout(handle);
    }
    designReloadPending.clear();
    designReloadLastFireAt.clear();
    clearDesignThrottle(designOptionsThrottle);
  };
}

interface QueueStateChangedPayload {
  threadId: string;
  items: Array<{
    id: string;
    threadId: string;
    message: string;
    attachmentIds?: string[];
    sourceProposedPlan?: SendQueueItem['sourceProposedPlan'];
    revisionSourceProposedPlan?: SendQueueItem['revisionSourceProposedPlan'];
    revisionSourceCommentIds?: string[];
    enqueuedAt: number;
  }>;
}

interface QueueFlushedPayload {
  threadId: string;
  items: Array<{ queueItemId: string; userItemId: string; message: string }>;
}

function applyQueueStateChanged(evt: QueueStateChangedPayload | undefined): void {
  if (!evt || !evt.threadId) return;
  const items: SendQueueItem[] = (evt.items ?? []).map((item) => ({
    id: item.id,
    threadId: item.threadId,
    message: item.message,
    attachmentIds: item.attachmentIds ? [...item.attachmentIds] : [],
    sourceProposedPlan: item.sourceProposedPlan ?? null,
    revisionSourceProposedPlan: item.revisionSourceProposedPlan ?? null,
    revisionSourceCommentIds: item.revisionSourceCommentIds
      ? [...item.revisionSourceCommentIds]
      : undefined,
    enqueuedAt: item.enqueuedAt,
  }));
  replaceQueueForThread(evt.threadId, items);
}

function applyQueueFlushed(evt: QueueFlushedPayload | undefined): void {
  if (!evt || !evt.threadId || !evt.items || evt.items.length === 0) return;
  markItemsFlushed(evt.threadId, evt.items);
}
