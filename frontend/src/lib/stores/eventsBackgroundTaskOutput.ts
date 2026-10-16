// The live tail of a background command's output
// (`provider:background_task_output`). The backend sends a thread's frames
// only to a client that watches the thread, and their one reader is the
// watch registry (stores/backgroundTaskOutput.svelte.ts), which applies
// them to the tails it holds for expanded tray rows. setupEventListeners
// routes the channel here.

import type { BackgroundTaskOutputEvent } from '../types/events';
import type { BackendKey } from '../transport/backendKey';
import { backendKeyForOrigin } from '../transport/backends';
import type { EventOrigin } from '../transport/handle';

/** A frame and the computer it came from; undefined when the origin is unknown. */
type BackgroundTaskOutputHandler = (evt: BackgroundTaskOutputEvent, origin: BackendKey | undefined) => void;

const subscribers = new Set<BackgroundTaskOutputHandler>();

export function onBackgroundTaskOutputEvent(handler: BackgroundTaskOutputHandler): () => void {
  subscribers.add(handler);
  return () => {
    subscribers.delete(handler);
  };
}

export function applyBackgroundTaskOutputEvent(
  evt: BackgroundTaskOutputEvent | undefined,
  origin?: EventOrigin,
): void {
  if (!evt || typeof evt.threadId !== 'string' || !evt.threadId) return;
  if (typeof evt.launchId !== 'string' || !evt.launchId) return;
  const backend = backendKeyForOrigin(origin?.backendId ?? '');
  for (const handler of subscribers) handler(evt, backend);
}
