// `thread:agent_rows_changed`: one spawn, send or ask a thread made of
// another thread changed, or the thread it went to did (turn, prompt, title,
// latest activity). The frame names the request's caller and carries the row
// as it stands, `removed` when it is no longer an agent, or neither when it
// is about every request of the caller, which asks a reader to read again. A
// lost frame on one computer may have been about any of its threads, so a
// gap asks every reader on that computer to read again. setupEventListeners
// and the gap router route the channel here.

import type { ThreadAgentRow } from './bindings';

/** One frame, as the backend's `ThreadAgentRowChange` sends it. */
export interface ThreadAgentRowChange {
  /** The thread that made the request. */
  threadId: string;
  /** The request's token; absent for a frame about every request. */
  id?: string;
  row?: ThreadAgentRow;
  removed?: boolean;
}

/**
 * `change` null: a frame for any thread of `backend` may have been lost, and
 * the reader reads every thread of that computer again.
 */
type ThreadAgentRowsHandler = (change: ThreadAgentRowChange | null, backend: string) => void;

const subscribers = new Set<ThreadAgentRowsHandler>();

export function onThreadAgentRowsChanged(handler: ThreadAgentRowsHandler): () => void {
  subscribers.add(handler);
  return () => {
    subscribers.delete(handler);
  };
}

export function applyThreadAgentRowsChanged(evt: Partial<ThreadAgentRowChange> | undefined, backend: string): void {
  if (!evt || typeof evt.threadId !== 'string' || !evt.threadId) return;
  const change: ThreadAgentRowChange = { threadId: evt.threadId };
  if (typeof evt.id === 'string' && evt.id) {
    change.id = evt.id;
    if (evt.row && typeof evt.row === 'object') change.row = evt.row;
    else if (evt.removed === true) change.removed = true;
  }
  for (const handler of subscribers) handler(change, backend);
}

export function applyThreadAgentRowsGap(backend: string): void {
  for (const handler of subscribers) handler(null, backend);
}
