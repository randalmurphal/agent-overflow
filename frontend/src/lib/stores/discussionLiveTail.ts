// Live-tail routing registry for discussion participant threads.
//
// A discussion child thread (one participant's own session) streams its
// in-flight `assistant_text` items over the ordinary `provider:item_event`
// channel, exactly like any other thread. But it has no mounted
// `ThreadPane` — only the parent thread gets a pane (rendering
// ChannelView) — so `eventsItemStream.ts`'s normal "match the event's
// threadId against a mounted pane" loop drops that traffic on the floor.
//
// This registry is the side-channel `eventsItemStream.ts` feeds instead:
// it maps a participant child-thread id to the channel-state instance(s)
// (bound `applyTailUpsert`/`applyTailDelta` closures) that want its live
// text. `threadChannelState.svelte.ts` registers its roster's child
// thread ids when a `discussion:state` snapshot lands and unregisters
// removed ids / on `clear()` — see its `applyState`.
//
// Deliberately rune-free: this is a plain module-level Map, not reactive
// state. The channel-state instance holding a registered handler is the
// reactive side; this registry is just routing.
import { refreshWatchedThreads, registerWatchedThreadSource } from './watchedThreads';

export interface DiscussionLiveTailHandler {
  /** Upserts carry the full accumulated text — replaces, doesn't append.
   * Also self-repairs a mid-turn mount that missed earlier deltas. */
  applyTailUpsert(threadId: string, itemId: string, fullText: string): void;
  /** Deltas carry an incremental chunk — appends to the current tail. */
  applyTailDelta(threadId: string, itemId: string, chunk: string): void;
}

const registry = new Map<string, Set<DiscussionLiveTailHandler>>();

// A registered child thread is a thread this client is looking at, even
// though no pane will ever hold it: its live text is rendered inside the
// parent's ChannelView. So the routing table is a watched-thread source in
// its own right (watchedThreads.ts) — without it, narrowing would withhold
// exactly the participant threads this side-channel exists to carry.
registerWatchedThreadSource(function* liveTailThreadIds() {
  yield* registry.keys();
});

/** Register `handler` to receive live-tail traffic for `threadId`. One
 * channel-state instance registers the SAME handler under every id in
 * its participant roster; multiple handlers can share one id (e.g. two
 * panes showing the same parent discussion thread). */
export function registerDiscussionLiveTail(
  threadId: string,
  handler: DiscussionLiveTailHandler,
): void {
  if (!threadId) return;
  let handlers = registry.get(threadId);
  if (!handlers) {
    handlers = new Set();
    registry.set(threadId, handlers);
  }
  handlers.add(handler);
  refreshWatchedThreads();
}

export function unregisterDiscussionLiveTail(
  threadId: string,
  handler: DiscussionLiveTailHandler,
): void {
  const handlers = registry.get(threadId);
  if (!handlers) return;
  handlers.delete(handler);
  if (handlers.size === 0) {
    registry.delete(threadId);
    refreshWatchedThreads();
  }
}

/** Read-only lookup for `eventsItemStream.ts`. `undefined` (not an empty
 * set) when nothing is registered, so callers can skip the feed entirely
 * on the hot path with one Map lookup and no allocation. */
export function lookupDiscussionLiveTail(
  threadId: string,
): ReadonlySet<DiscussionLiveTailHandler> | undefined {
  return registry.get(threadId);
}

/** Full-registry teardown. Mirrors `clearAllDesignThrottles` — called
 * from `events.ts`'s `setupEventListeners` cleanup so a torn-down and
 * re-attached listener set starts from a clean slate. */
export function clearAllDiscussionLiveTail(): void {
  registry.clear();
  refreshWatchedThreads();
}
