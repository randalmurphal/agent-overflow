// Send-queue mirror event domain: applying the backend's authoritative
// per-thread queue snapshots (state changed / flushed / restored) onto the
// frontend's Zone 1 send-queue store. Fan-in target of events.ts's
// setupEventListeners.
import {
  markItemsFlushed,
  queueItemFromWire,
  removeRestoredQueueItems,
  replaceQueueForThread,
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
