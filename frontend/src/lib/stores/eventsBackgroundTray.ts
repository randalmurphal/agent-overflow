// The background tray's deltas (`provider:background_tray`). The backend
// sends a thread's frames only to a client that watches the thread, and
// their one reader is that thread's tray controller
// (components/composer/activityRailBackground.svelte.ts), which applies
// them to the snapshot it reads from ListLiveBackgroundTasks.
// setupEventListeners routes the channel here.

import type { BackgroundTrayEvent } from '../types/events';

type BackgroundTrayHandler = (evt: BackgroundTrayEvent) => void;

const subscribers = new Set<BackgroundTrayHandler>();

export function onBackgroundTrayEvent(handler: BackgroundTrayHandler): () => void {
  subscribers.add(handler);
  return () => {
    subscribers.delete(handler);
  };
}

export function applyBackgroundTrayEvent(evt: BackgroundTrayEvent | undefined): void {
  if (!evt || typeof evt.threadId !== 'string' || !evt.threadId) return;
  for (const handler of [...subscribers]) handler(evt);
}
