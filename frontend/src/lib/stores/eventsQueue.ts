// Send-queue mirror event domain: applying the backend's authoritative
// per-thread queue snapshots (state changed / flushed / restored) onto the
// frontend's Zone 1 send-queue store. Fan-in target of events.ts's
// setupEventListeners.
import {
  applyFlushedLifecycle,
  flushMarkAt,
  markItemsFlushed,
  queueItemFromWire,
  releaseSettledFlushed,
  removeRestoredQueueItems,
  replaceQueueForThread,
  sameFlushStream,
  type FlushedLifecycle,
  type FlushMark,
} from './sendQueue.svelte';
import type { QueuedItem as WireQueuedItem } from '../../../bindings/agent-overflow/internal/app/models';
import { iterPanes } from './panes.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import type { ThreadPaneIngest } from './threadPaneRoles';
import { applySendsPendingFrame } from './threadStatuses.svelte';
import type { BackendKey } from '../transport/backendKey';
import { onBackendDetached } from '../transport/backends';
import { currentThreadEvent } from '../transport/entityIndex';
import { threadHasScope } from '../transport/entityScopes';

// The registry hands out whole ThreadPanes; this module narrows them to
// the ingest surface at the one acquisition point, so a new pane member
// use here fails to compile until threadPaneRoles.ts lists it.
function ingestPanes(): Iterable<ThreadPaneIngest> {
  return iterPanes();
}

function threadHasPane(threadId: string): boolean {
  for (const pane of ingestPanes()) {
    if (pane.threadId === threadId) return true;
  }
  return false;
}

// A `provider:sends_pending` frame that reads nothing pending settles the
// thread's flushes up to its `flushedThrough`, a `provider:queue_flushed`
// sequence. Live frames arrive in the order the backend emitted them, so
// the flush frames a settle covers have arrived. A replay hands each
// channel over whole, in no fixed order, so a settle can arrive first.
// `flushStreams` is the flush frame each backend's launch has reached, and
// `settledAhead` keeps a settle that is ahead of it until the stream
// catches up, so the entries its late flush frames add are released too.
const flushStreams = new Map<BackendKey, FlushMark>();
const settledAhead = new Map<string, FlushMark>();

function reachFlushStream(mark: FlushMark): void {
  const stream = flushStreams.get(mark.backend);
  if (stream && sameFlushStream(stream, mark) && stream.sequence >= mark.sequence) return;
  flushStreams.set(mark.backend, mark);
  for (const [threadId, settled] of settledAhead) {
    if (settled.backend !== mark.backend) continue;
    if (!sameFlushStream(settled, mark) || settled.sequence <= mark.sequence) settledAhead.delete(threadId);
  }
}

onBackendDetached(({ backendId }) => {
  flushStreams.delete(backendId);
  for (const [threadId, settled] of settledAhead) {
    if (settled.backend === backendId) settledAhead.delete(threadId);
  }
});

/** How many backends' flush streams and threads' settles the ordering retains. */
export function __flushSettleOrderForTest(): { streams: number; ahead: number } {
  return { streams: flushStreams.size, ahead: settledAhead.size };
}

export function __resetFlushSettleOrderForTest(): void {
  flushStreams.clear();
  settledAhead.clear();
}

export interface QueueStateChangedPayload {
  threadId: string;
  items: WireQueuedItem[];
}

export interface QueueFlushedPayload {
  threadId: string;
  items: Array<{ queueItemId: string; userItemId: string; message: string; sendId?: string }>;
}

/** `provider:sends_pending`: whether the thread holds a message the
 * provider has not echoed yet. Reaches every client, pane or not. */
export interface SendsPendingPayload {
  threadId: string;
  pending: boolean;
  /** The newest `provider:queue_flushed` sequence the answer saw. Absent
   * from a backend that does not report it. */
  flushedThrough?: number;
}

export interface QueueRestoredPayload {
  threadId: string;
  reason: string;
  queueItemIds?: string[];
  userItemIds?: string[];
}

/** `provider:command_lifecycle` — the provider's delivery ack for a
 * message AO wrote to its stdin. Claude-only and CLI-version-dependent;
 * a session that never emits these leaves Zone 2 exactly as it was
 * before this channel existed. */
export interface CommandLifecyclePayload {
  threadId: string;
  commandUuid: string;
  userItemId?: string;
  state: FlushedLifecycle['state'];
  delivery?: FlushedLifecycle['delivery'];
}

export function applyCommandLifecycle(evt: CommandLifecyclePayload | undefined): void {
  if (!evt || !evt.threadId || !evt.state) return;
  // No userItemId means the backend could not correlate the ack to a row
  // (a uuid from a previous process, or one it never registered). There
  // is nothing to attach it to, and guessing would attach it to the
  // wrong message.
  if (!evt.userItemId) return;
  applyFlushedLifecycle(evt.threadId, evt.userItemId, {
    state: evt.state,
    delivery: evt.delivery,
  });
}

export function applyQueueStateChanged(evt: QueueStateChangedPayload | undefined): void {
  if (!evt || !evt.threadId) return;
  const items = (evt.items ?? []).map(queueItemFromWire);
  replaceQueueForThread(evt.threadId, items);
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    for (const item of items) pane.confirmOptimisticSend(evt.threadId, item.sendId);
  }
}

// A gap marker on the channel arrives here too, with no payload: its
// sequence moves the stream past the frames it lost.
export function applyQueueFlushed(
  evt: QueueFlushedPayload | null | undefined,
  backend?: BackendKey,
  sequence?: number,
): void {
  const mark = backend === undefined ? undefined : flushMarkAt(backend, sequence);
  if (evt && evt.threadId && evt.items && evt.items.length > 0) addFlushed(evt, mark);
  if (mark) reachFlushStream(mark);
}

function addFlushed(evt: QueueFlushedPayload, mark: FlushMark | undefined): void {
  markItemsFlushed(evt.threadId, evt.items, mark);
  const settled = settledAhead.get(evt.threadId);
  if (mark && settled && sameFlushStream(settled, mark) && !threadHasPane(evt.threadId)) {
    releaseSettledFlushed(evt.threadId, settled);
  }
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    for (const item of evt.items) {
      pane.confirmOptimisticSend(evt.threadId, item.sendId, item.userItemId);
    }
    // The row can already be on screen when this frame lands: the eager
    // quiet persist emits `queue_flushed` before its `item_event`, but a
    // replay off the event ring re-delivers the frame long after the row
    // mounted. `markItemsFlushed` adds unconditionally (it cannot know),
    // and the pane takes back what it renders — in this same synchronous
    // handler, so Zone 2 and the timeline are never both showing it.
    pane.syncRenderedFlushRows();
  }
}

export function applyQueueRestored(evt: QueueRestoredPayload | undefined): void {
  if (!evt || !evt.threadId) return;
  removeRestoredQueueItems(evt.threadId, {
    queueItemIds: evt.queueItemIds ?? [],
    userItemIds: evt.userItemIds ?? [],
  });
  const userItemIds = evt.userItemIds ?? [];
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    // The backend deleted these rows when it restored their content to
    // the draft (failed Codex resend, session death); drop any mounted
    // timeline rows so the UI matches the store.
    for (const id of userItemIds) pane.removeItemById(id, evt.threadId);
    const draft = getComposerDraftForPane(pane.paneId);
    if (draft) {
      void draft.reloadFromBackend(evt.threadId);
    }
  }
}

// A gap marker on the channel arrives here too, with no payload: its
// sequence is what orders RegisterQueueItem's answers after the loss.
export function applySendsPendingEvent(
  evt: SendsPendingPayload | null | undefined,
  backend: BackendKey,
  sequence: number | undefined,
): void {
  releaseSettledFlushes(evt, backend);
  applySendsPendingFrame(evt, backend, sequence);
}

// Runs for every frame, including one the status ordering drops as older
// than a RegisterQueueItem answer: what a frame settles stays settled.
function releaseSettledFlushes(evt: SendsPendingPayload | null | undefined, backend: BackendKey): void {
  if (!evt?.threadId || evt.pending || !currentThreadEvent(evt.threadId, backend)) return;
  const settled = flushMarkAt(backend, evt.flushedThrough);
  if (!settled) return;
  const stream = flushStreams.get(backend);
  const reached = stream !== undefined && sameFlushStream(stream, settled) && stream.sequence >= settled.sequence;
  // Flush frames reach only a client that may operate the thread.
  if (!reached && threadHasScope('threads:operate', evt.threadId)) settledAhead.set(evt.threadId, settled);
  if (!threadHasPane(evt.threadId)) releaseSettledFlushed(evt.threadId, settled);
}
