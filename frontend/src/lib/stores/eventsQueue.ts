// Send-queue mirror event domain: applying the backend's authoritative
// per-thread queue snapshots (state changed / flushed / restored) onto the
// frontend's Zone 1 send-queue store. Fan-in target of events.ts's
// setupEventListeners.
import {
  applyFlushedLifecycle,
  markItemsFlushed,
  queueItemFromWire,
  removeRestoredQueueItems,
  replaceQueueForThread,
  type FlushedLifecycle,
} from './sendQueue.svelte';
import type { QueuedItem as WireQueuedItem } from '../../../bindings/agent-overflow/models';
import { iterPanes } from './panes.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';

export interface QueueStateChangedPayload {
  threadId: string;
  items: WireQueuedItem[];
}

export interface QueueFlushedPayload {
  threadId: string;
  items: Array<{ queueItemId: string; userItemId: string; message: string }>;
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
}

export function applyQueueFlushed(evt: QueueFlushedPayload | undefined): void {
  if (!evt || !evt.threadId || !evt.items || evt.items.length === 0) return;
  markItemsFlushed(evt.threadId, evt.items);
}

export function applyQueueRestored(evt: QueueRestoredPayload | undefined): void {
  if (!evt || !evt.threadId) return;
  removeRestoredQueueItems(evt.threadId, {
    queueItemIds: evt.queueItemIds ?? [],
    userItemIds: evt.userItemIds ?? [],
  });
  const userItemIds = evt.userItemIds ?? [];
  for (const pane of iterPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    // The backend deleted these rows when it restored their content to
    // the draft (failed Codex resend, session death); drop any mounted
    // timeline rows so the UI matches the store.
    for (const id of userItemIds) pane.removeItemById(id);
    const draft = getComposerDraftForPane(pane.paneId);
    if (draft) {
      void draft.reloadFromBackend(evt.threadId);
    }
  }
}
