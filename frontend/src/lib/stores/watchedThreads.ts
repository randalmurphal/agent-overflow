// Composes the set of threads this client is looking at and pushes it to
// the transport, which narrows the entity-filtered channels to it
// (lib/transport/entityFilteredChannels.ts).
//
// EXISTENCE, NEVER VISIBILITY. A thread is watched because a surface for it
// exists, not because that surface is on screen, focused, or in a visible
// document. An off-screen pane, a background tab and a hidden window all
// keep watching: a surface that stopped receiving would render stale the
// instant it is looked at again, and recovering from that is a resync the
// user waits through. Nothing here may ever read `document.hidden`,
// `IntersectionObserver`, or which pane has focus.
//
// Deliberately rune-free and source-registered rather than importing the
// stores it reads. Two surfaces contribute threads today and they sit at
// different levels (the pane registry, and the discussion live-tail routing
// table that participant CHILD threads have no pane in), so this module
// stays a leaf that only knows how to union what it is handed — the same
// one-way shape panes.svelte.ts uses for its destroyed/mounted observers.
//
// The set is composed here and SPLIT in transport/backends.ts: a watch
// frame narrows one connection, and once a client is attached to several
// machines a thread that lives on one of them is nothing the others can
// push about. This module deliberately does not know which machine owns
// what — sending to the home socket alone is what it used to do, and it
// left every pane on an attached machine receiving nothing.
import { setWatchedThreadsEverywhere } from '../transport/backends';

type WatchedThreadSource = () => Iterable<string>;

const sources = new Set<WatchedThreadSource>();

/**
 * Register a contributor of watched thread ids. Every registered source is
 * asked on each recompute, and the union is what reaches the wire.
 *
 * A source that returns a thread nothing renders costs wire bytes; a
 * surface that consumes a narrowed channel and registers NO source stops
 * receiving. So the rule for adding one is the same as for adding a row to
 * the backend's EntityFiltered column, read from the other side: if a
 * surface consumes an entity-filtered channel for a thread, its ids belong
 * here.
 */
export function registerWatchedThreadSource(source: WatchedThreadSource): () => void {
  sources.add(source);
  refreshWatchedThreads();
  return () => {
    sources.delete(source);
    refreshWatchedThreads();
  };
}

function composeWatchedThreads(): string[] {
  const ids: string[] = [];
  for (const source of sources) {
    for (const id of source()) {
      if (id) ids.push(id);
    }
  }
  return ids;
}

/**
 * Recompute the watched set from every registered source and push it. The
 * transport dedups, so calling this after any composition change is cheap
 * and calling it redundantly costs a small array build and nothing on the
 * wire.
 */
export function refreshWatchedThreads(): void {
  setWatchedThreadsEverywhere(composeWatchedThreads());
}

/**
 * Add `threadIds` to the watched set immediately, ahead of the mounts that
 * will make them derivable from the sources.
 *
 * This exists for ordering, not for state. A pane's thread only becomes
 * visible to `composeWatchedThreads` after `switchThread` resolves — which
 * is after that thread's history and window loads have already gone out on
 * this socket — so a set composed only at that point would leave a window
 * where the backend is withholding the very frames the newly opened pane
 * needs. Sending the union first closes it, and the ordinary recompute
 * after the mount restates the authoritative set.
 *
 * Nothing is retained: a mount that then fails leaves one extra thread in
 * the SENT set, which the next recompute corrects, and which costs only
 * wire bytes in the meantime. That is the direction this is allowed to be
 * wrong in.
 */
export function watchThreadsBeforeMount(threadIds: readonly string[]): void {
  if (threadIds.length === 0) return;
  setWatchedThreadsEverywhere([...composeWatchedThreads(), ...threadIds]);
}

/** Test seam: drops every registered source and the composed set with it. */
export function resetWatchedThreadSourcesForTest(): void {
  sources.clear();
}
