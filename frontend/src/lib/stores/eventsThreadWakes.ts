// `thread:wakes_changed`: one loop, watch or reminder of a thread changed
// (registered, fired, ran, recorded an outcome or an undelivered wake,
// cancelled). The frame carries the row as it stands, `removed` when it left
// the listing, or neither when the backend could not read it, which asks a
// reader to read the thread again. A lost frame on one computer may have been
// about any of its threads, so a gap asks every reader on that computer to
// read again. setupEventListeners and the gap router route the channel here.

import type { ThreadWakeRow } from './bindings';

/** One frame, as the backend's `ThreadWakeChange` sends it. */
export interface ThreadWakeChange {
  threadId: string;
  /** The loop or watch id, or the reminder's token. */
  id: string;
  row?: ThreadWakeRow;
  removed?: boolean;
}

/**
 * `change` null: a frame for any thread of `backend` may have been lost, and
 * the reader reads every thread of that computer again.
 */
type ThreadWakesHandler = (change: ThreadWakeChange | null, backend: string) => void;

const subscribers = new Set<ThreadWakesHandler>();

export function onThreadWakesChanged(handler: ThreadWakesHandler): () => void {
  subscribers.add(handler);
  return () => {
    subscribers.delete(handler);
  };
}

export function applyThreadWakesChanged(evt: Partial<ThreadWakeChange> | undefined, backend: string): void {
  if (!evt || typeof evt.threadId !== 'string' || !evt.threadId || typeof evt.id !== 'string' || !evt.id) return;
  const change: ThreadWakeChange = { threadId: evt.threadId, id: evt.id };
  if (evt.row && typeof evt.row === 'object') change.row = evt.row;
  else if (evt.removed === true) change.removed = true;
  for (const handler of subscribers) handler(change, backend);
}

export function applyThreadWakesGap(backend: string): void {
  for (const handler of subscribers) handler(null, backend);
}
