// `thread:wakes_changed`: a thread's loops, watches or reminders changed
// (registered, fired, ran, recorded an outcome, cancelled). The frame names
// only the thread; its readers read ListThreadWakes again. A lost frame on
// one computer may have been about any of its threads, so a gap asks every
// reader on that computer to read again. setupEventListeners and the gap
// router route the channel here.

/** threadId null: a frame for any thread of `backend` may have been lost. */
type ThreadWakesHandler = (threadId: string | null, backend: string) => void;

const subscribers = new Set<ThreadWakesHandler>();

export function onThreadWakesChanged(handler: ThreadWakesHandler): () => void {
  subscribers.add(handler);
  return () => {
    subscribers.delete(handler);
  };
}

export function applyThreadWakesChanged(evt: { threadId?: unknown } | undefined, backend: string): void {
  if (!evt || typeof evt.threadId !== 'string' || !evt.threadId) return;
  for (const handler of subscribers) handler(evt.threadId, backend);
}

export function applyThreadWakesGap(backend: string): void {
  for (const handler of subscribers) handler(null, backend);
}
