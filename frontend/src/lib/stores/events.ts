import { Events } from '@wailsio/runtime';
import type {
  ApprovalEvent,
  ItemDeltaEvent,
  ItemStreamEvent,
  ProviderStatusEvent,
  SubagentNotificationEvent,
  TurnCompletedEvent,
  TurnStartedEvent,
  UsageEvent,
  UserInputEvent,
} from '../types/events';
import type { Item, Thread } from '../types/models';
import type { DesignArtifact, DesignChoiceResolved, DesignOptionsRequest } from '../types/design';
import { getAllPanes } from './panes.svelte';
import { recordProviderStatus } from './providerStatus.svelte';
import { addToast } from './toast.svelte';
import { getThreadById, getThreads, replaceThread } from './threads.svelte';
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
 * SeqEnvelope mirrors the Go-side `SeqEnvelope` in app.go. Every Wails
 * emission stamps a monotonic `seq` so subscribers can detect gaps
 * (scaffolding for future remote-access transport). The envelope also
 * carries a `data` field with the original Go payload.
 *
 * We keep the detection shape duck-typed instead of using `instanceof`
 * or a JSON-schema check — the Go runtime serialises the envelope
 * through encoding/json, so what arrives in the webview is a plain
 * object with `seq` + `data` keys.
 */
interface SeqEnvelope<T = unknown> {
  seq: number;
  data: T;
}

function isSeqEnvelope(value: unknown): value is SeqEnvelope {
  return (
    value !== null
    && typeof value === 'object'
    && 'seq' in value
    && typeof (value as { seq: unknown }).seq === 'number'
    && 'data' in value
  );
}

/**
 * Per-event-name last-seen seq table. Keys are event names; values are
 * the most recent seq observed on that channel. Undefined = never seen,
 * so the first emission on a channel does not warn.
 *
 * We reset the table when setupEventListeners() is called again — tests
 * re-install listeners between cases, and a stale last-seen would
 * trigger spurious gap warnings.
 */
const lastSeenSeq: Map<string, number> = new Map();
const itemUpsertSubscribers: Set<(item: Item) => void> = new Set();
const ITEM_EVENT_FLUSH_MAX_DELAY_MS = 50;
const ITEM_EVENT_FLUSH_MAX_EVENTS = 500;
const ITEM_EVENT_QUEUE_FORCE_FLUSH_EVENTS = 2_000;
const ITEM_EVENT_TEXT_FIELD_MAX_CHARS = 2_000_000;
let itemEventQueue: ItemStreamEvent[] = [];
let itemEventQueueStart = 0;
let itemEventFlushFrame: number | null = null;
let itemEventFlushTimeout: number | null = null;

function resetSeqTracking(): void {
  lastSeenSeq.clear();
}

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

function requiresSeqEnvelope(name: string): boolean {
  return name.startsWith('provider:');
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
 * wailsEventOn wraps Events.On to (a) unwrap SeqEnvelope payloads back
 * to the original Go shape, and (b) track per-channel seq gaps. The
 * Provider channels must arrive in the envelope so the frontend can
 * enforce the ordering/gap contract. Non-provider app-shell channels may
 * still pass raw payloads through this helper.
 *
 * Exported so subscribers outside this file (terminal drawer, diff
 * panel) can share the same gap-detection + unwrapping logic without
 * re-implementing the boilerplate.
 */
export function wailsEventOn<T = unknown>(
  name: string,
  handler: (data: T) => void,
): () => void {
  return Events.On(name, (ev) => {
    const raw = ev.data as unknown;
    if (isSeqEnvelope(raw)) {
      const prev = lastSeenSeq.get(name);
      // A strictly-increasing seq means no gap; anything else (drop,
      // retransmit, out-of-order) produces a warn with the missing
      // range. We warn once per gap and still deliver the event — the
      // seq is an observability scaffold, not a back-pressure knob.
      if (prev !== undefined && raw.seq > prev + 1) {
        const missingCount = raw.seq - prev - 1;
        const firstMissing = prev + 1;
        const lastMissing = raw.seq - 1;
        console.warn(
          `event seq gap on ${name}: missing ${missingCount} event(s) ` +
          `(ids ${firstMissing}..${lastMissing})`,
        );
      }
      // Track the highest seq we've seen so a stale re-delivery doesn't
      // roll the pointer backward.
      if (prev === undefined || raw.seq > prev) {
        lastSeenSeq.set(name, raw.seq);
      }
      handler(raw.data as T);
      return;
    }
    if (requiresSeqEnvelope(name)) {
      console.warn(`dropping unenveloped provider event on ${name}`);
      return;
    }
    // Raw payload: pass through unchanged for app-shell channels that do
    // not participate in provider stream ordering.
    handler(raw as T);
  });
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
  const merged = { ...updated, lastReadAt, latestTurnCompletedAt };
  replaceThread(merged);
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== updated.id || !pane.thread) continue;
    pane.replaceThread(merged);
  }
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
      if (!hadApproval) continue;
      pane.removeApproval(evt.requestId);
      if (evt.action === 'fail' && evt.detail) {
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
      if (!hadRequest) continue;
      pane.removeUserInput(evt.requestId);
      if (evt.action === 'fail' && evt.detail) {
        pane.setGeneralError(`Failed to submit input: ${evt.detail}`);
      }
    }
  }
}

function applyUsageEvent(evt: UsageEvent): void {
  if (!evt?.threadId) return;

  // `rate_limits` piggybacks on the same channel but doesn't touch the
  // context-window ring — the popover renders it as a separate row. Bail
  // before the ring-update path so a rate-limit refresh never clobbers
  // the last known token-window snapshot.
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
  }
  for (const pane of getAllPanes().values()) {
    const threadItems = pane.threadId ? itemsByThread.get(pane.threadId) : undefined;
    if (!threadItems) continue;
    pane.upsertItems(threadItems);
  }
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
// `rate_limited_retrying` and `transient_retry` both land on
// `version_too_old` (the banner's warning-styled branch) rather than
// `error` (red / terminal-failure styling). "Please wait, we're retrying"
// is warning, not catastrophic. The banner copy still comes from the
// event `message` so the UX is accurate without having to teach
// ProviderStatusBanner a new branch.
const KIND_TO_LEGACY_STATUS: Record<NonNullable<ProviderStatusEvent['kind']>, ProviderStatusEvent['status']> = {
  binary_missing: 'not_found',
  unauthenticated: 'unauthenticated',
  version_incompatible: 'version_too_old',
  rate_limited_retrying: 'version_too_old',
  transient_retry: 'version_too_old',
  ok: 'ready',
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
 * Route `provider:turn_started` to the matching pane. Flips
 * `pane.activeTurn` on so the composer blocks sends and (Wave 3) the
 * working indicator lights up. Invariant 22: this is the only way
 * `activeTurn` can become non-null.
 */
function applyTurnStarted(evt: TurnStartedEvent): void {
  if (!evt?.threadId || !evt.turnId) return;
  // Feed the sidebar live-status projection first so the pill flips to
  // Working the moment the backend confirms the turn has started —
  // before we wait on any streaming item deltas. Matches forge's
  // behavior and fixes the "idle during thinking" gap.
  projectTurnStarted(evt.threadId, evt.turnId);
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.setActiveTurn({
      turnId: evt.turnId,
      turnIndex: evt.turnIndex,
      startedAt: evt.startedAt,
    });
  }
}

/**
 * Route `provider:turn_completed` to the matching pane. Clears
 * `pane.activeTurn` and writes the settled projection so (Wave 3) the
 * completion divider can render above the final assistant message.
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
  // Clear the turn from the sidebar projection. Aborted / errored
  // turns flip the pill to Error so the row surfaces the failure
  // without the user having to open the thread.
  projectTurnCompleted(evt.threadId, evt.turnId, {
    aborted: settled.aborted,
    errorMessage: settled.errorMessage,
  });
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.settleTurn(settled);
  }
  syncLatestTurnCompleted(evt);
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
 * Set up the app's Wails event listeners.
 * Returns a cleanup function that removes all listeners.
 */
export function setupEventListeners(): () => void {
  // Reset the gap-detection table so a previous setupEventListeners call
  // (tests re-wire between cases) does not bleed its last-seen seq into
  // the new listener set and trigger spurious warnings.
  resetSeqTracking();
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
  // These are the sole drivers of `pane.activeTurn` /
  // `pane.latestSettledTurn`. See invariant 22 and
  // docs/architecture/turn-lifecycle.md §Frontend state shape.
  const cancelTurnStarted = wailsEventOn<TurnStartedEvent>('provider:turn_started', applyTurnStarted);
  const cancelTurnCompleted = wailsEventOn<TurnCompletedEvent>('provider:turn_completed', applyTurnCompleted);
  // provider:subagent_notification — Codex passes subagent metadata
  // through; no UI renders this yet, but the pane records it so future
  // surfaces can subscribe without re-wiring.
  const cancelSubagentNotification = wailsEventOn<SubagentNotificationEvent>(
    'provider:subagent_notification',
    applySubagentNotification,
  );

  const cancelThreadUpdated = wailsEventOn<Thread>('thread:updated', applyThreadUpdated);

  // design:artifact — a new rendered artifact. Append to the owning pane's
  // history. The preview panel auto-tracks the latest unless the user has
  // pinned a specific artifact via the dropdown.
  const cancelDesignArtifact = wailsEventOn<DesignArtifact>('design:artifact', (artifact) => {
    if (!artifact || !artifact.threadId) return;
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === artifact.threadId) {
        pane.appendDesignArtifact(artifact);
      }
    }
  });

  // design:options — agent blocked on present_options. Also append the option
  // artifacts to history so the picker thumbnails resolve without a round-trip.
  const cancelDesignOptions = wailsEventOn<DesignOptionsRequest>('design:options', (request) => {
    if (!request || !request.threadId) return;
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === request.threadId) {
        pane.setDesignOptions(request);
      }
    }
  });

  // design:chosen — user picked an option, backend resolved. Clear the
  // pending-options state. The corresponding artifact stays in history.
  const cancelDesignChosen = wailsEventOn<DesignChoiceResolved>('design:chosen', (resolved) => {
    if (!resolved || !resolved.threadId) return;
    for (const pane of getAllPanes().values()) {
      if (pane.threadId !== resolved.threadId) continue;
      const current = pane.pendingDesignOptions;
      // Only clear if this resolution matches the currently-pending request.
      // A stale `chosen` event for an older request shouldn't wipe a newer
      // pending picker.
      if (current && current.requestId === resolved.requestId) {
        pane.clearDesignOptions();
      }
    }
  });

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
    cancelSubagentNotification();
    cancelThreadUpdated();
    cancelDesignArtifact();
    cancelDesignOptions();
    cancelDesignChosen();
    cancelModeChanged();
    cancelRuntimeModeChanged();
  };
}
