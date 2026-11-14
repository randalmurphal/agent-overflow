// The Conversation section's feed state, owned by a review pane's state
// (reviewPane.svelte.ts) so it outlives the overview row unmounting.
//
// The feed is the live universe (threads, verdicts, events, pushes)
// in a held order: an entry keeps its place once shown, and every
// arrival joins at its chronological place among the entries around it.
// Nothing waits to be revealed. An arrival that joins while the reader's
// place is held (the section scrolled past its top, or focus inside it)
// counts toward the "N new" chip, which only points at entries already
// in the feed; an arrival the reader is at the top for, or one an
// explicit refresh read, does not. The section component holds the
// reader's pixel position across arrivals (ReviewConversation) from the
// anchor recorded here.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import type { ReviewThread } from '../types/models';
import type { ConversationFeedItem } from './reviewPane.svelte';

export interface ConversationFeedEntry {
  readonly timeMs: number;
  readonly item: ConversationFeedItem;
}

/** The entry at the top of the section's viewport and how far its top
 * sits from the viewport's top (negative when scrolled partly past). */
export interface ConversationFeedAnchor {
  readonly id: string;
  readonly offset: number;
}

export interface ConversationFeedHost {
  /** The live universe, chronological, newest first. */
  source(): readonly ConversationFeedEntry[];
  /** The PR the feed describes; a change starts a fresh view. */
  subjectKey(): string | null;
  /** The section's reading offset is past its top. */
  scrolled(): boolean;
  /** A thread the view hides (Hide resolved threads). */
  hidden(threadId: string): boolean;
}

export interface ConversationFeedState {
  readonly open: boolean;
  setOpen(open: boolean): void;
  /** The entries shown, in the held order. Empty while closed. */
  readonly feed: readonly ConversationFeedItem[];
  /** Arrivals the reader has not been shown: the chip's count. */
  readonly newCount: number;
  /** The newest of those arrivals, which the chip scrolls to. */
  readonly newestArrivalId: string | null;
  /** Every arrival counts as seen: the newest scrolled into view. */
  acknowledgeArrivals(): void;
  /** The chip's press: stage a scroll to the newest arrival and clear. */
  showNewestArrival(): void;
  /** The entry a chip press asked the section to scroll to. */
  readonly pendingArrivalId: string | null;
  consumePendingArrival(): void;
  /** The mounted section answers whether focus is inside it; returns
   * the detach. Unmounted, focus is not inside. */
  attachFocusProbe(probe: () => boolean): () => void;
  readonly anchor: ConversationFeedAnchor | null;
  setAnchor(anchor: ConversationFeedAnchor | null): void;
  /** An explicit re-read of the PR's threads starts: what lands while it
   * runs joins without the chip. */
  beginExplicitRead(): void;
  /** It ended; `answered` when its answer was applied, which shows
   * everything up to it and clears the chip. */
  endExplicitRead(answered: boolean): void;
  /** The viewer's own PR comment, named by the post's answer: it is in
   * the feed at its place, counts as seen, and the section scrolls to it. */
  joinPostedComment(commentId: string): void;
  /** Bring the feed up to the universe now (a jump that must land on an
   * entry that just arrived). */
  sync(): void;
  threadExpanded(threadId: string): boolean;
  toggleThread(threadId: string): void;
  expandThread(threadId: string): void;
  /** Forget the view: the order, the reply folds and the chip. */
  reset(): void;
  dispose(): void;
}

const EMPTY_IDS: ReadonlySet<string> = Object.freeze(new Set<string>());
const EMPTY_FEED: readonly ConversationFeedItem[] = Object.freeze([]);

// Replies of a settled thread fold by default.
function unfoldsByDefault(item: ConversationFeedItem): boolean {
  return item.kind === 'thread' && !(item.thread.isResolvable && item.thread.isResolved);
}

// The feed's order: newest first, ties by id.
function before(a: ConversationFeedEntry, b: { timeMs: number; id: string }): boolean {
  return a.timeMs !== b.timeMs ? a.timeMs > b.timeMs : a.item.id.localeCompare(b.id) < 0;
}

/** `order` with each arrival placed before the first entry it is newer
 * than; entries already in `order` keep their places. */
export function insertArrivals(order: readonly string[], source: readonly ConversationFeedEntry[]): string[] {
  const known = new Set(order);
  const time = new Map(source.map((entry) => [entry.item.id, entry.timeMs]));
  const next = [...order];
  for (const entry of source) {
    if (known.has(entry.item.id)) continue;
    let at = next.length;
    for (let index = 0; index < next.length; index += 1) {
      const timeMs = time.get(next[index]);
      if (timeMs === undefined) continue;
      if (before(entry, { timeMs, id: next[index] })) {
        at = index;
        break;
      }
    }
    next.splice(at, 0, entry.item.id);
    known.add(entry.item.id);
  }
  return next;
}

export function createConversationFeed(host: ConversationFeedHost): ConversationFeedState {
  let open = $state(false);
  let order: readonly string[] = $state.raw([]);
  let viewKey: string | null = null;
  // Entries the reader has been shown or did not need pointing at.
  let seen: ReadonlySet<string> = $state.raw(EMPTY_IDS);
  let defaultExpanded: ReadonlySet<string> = $state.raw(EMPTY_IDS);
  const expandOverrides = new SvelteMap<string, boolean>();
  let focusProbe: (() => boolean) | null = null;
  let explicitReads = $state(0);
  let anchor: ConversationFeedAnchor | null = $state.raw(null);
  let pendingArrivalId: string | null = $state(null);

  const feed = $derived.by<readonly ConversationFeedItem[]>(() => {
    if (!open) return EMPTY_FEED;
    const source = host.source();
    if (source.length === 0) return EMPTY_FEED;
    const shown = (item: ConversationFeedItem) => item.kind !== 'thread' || !host.hidden(item.thread.id);
    // Before the first sync (the same flush) the live order is the order.
    if (order.length === 0) return source.map((entry) => entry.item).filter(shown);
    const byId = new Map(source.map((entry) => [entry.item.id, entry.item]));
    const out: ConversationFeedItem[] = [];
    for (const id of order) {
      const item = byId.get(id);
      if (item && shown(item)) out.push(item);
    }
    return out;
  });

  // While the reader's refresh runs the chip waits for its answer, which
  // shows everything up to it.
  const unseen = $derived(explicitReads > 0 ? EMPTY_FEED : feed.filter((item) => !seen.has(item.id)));

  function held(): boolean {
    return host.scrolled() || (focusProbe?.() ?? false);
  }

  function reset(): void {
    order = [];
    seen = EMPTY_IDS;
    defaultExpanded = EMPTY_IDS;
    expandOverrides.clear();
    anchor = null;
    pendingArrivalId = null;
  }

  function acknowledgeArrivals(): void {
    if (order.every((id) => seen.has(id))) return;
    seen = new Set(order);
  }

  // Folds the universe into the held order. Untracked: it runs from the
  // watcher below and from the calls that must see an arrival at once.
  function sync(): void {
    untrack(() => {
      const key = host.subjectKey();
      if (key !== viewKey) {
        viewKey = key;
        reset();
      }
      if (!open) return;
      const source = host.source();
      if (order.length === 0) {
        if (source.length === 0) return;
        order = source.map((entry) => entry.item.id);
        seen = new Set(order);
        defaultExpanded = new Set(source.filter((entry) => unfoldsByDefault(entry.item)).map((entry) => (entry.item as { thread: ReviewThread }).thread.id));
        return;
      }
      const known = new Set(order);
      const arrivals = source.filter((entry) => !known.has(entry.item.id));
      if (arrivals.length === 0) return;
      order = insertArrivals(order, source);
      const unfolds = arrivals.filter((entry) => unfoldsByDefault(entry.item));
      if (unfolds.length > 0) {
        const next = new Set(defaultExpanded);
        for (const entry of unfolds) if (entry.item.kind === 'thread') next.add(entry.item.thread.id);
        defaultExpanded = next;
      }
      if (!held()) {
        const next = new Set(seen);
        for (const entry of arrivals) next.add(entry.item.id);
        seen = next;
      }
    });
  }

  const disposeWatch = $effect.root(() => {
    $effect(() => {
      void host.source();
      void host.subjectKey();
      void open;
      sync();
    });
  });

  function threadExpanded(threadId: string): boolean {
    return expandOverrides.get(threadId) ?? defaultExpanded.has(threadId);
  }

  return {
    get open() { return open; },
    setOpen(next: boolean): void {
      if (next === open) return;
      open = next;
      if (next) sync();
      else reset();
    },
    get feed() { return feed; },
    get newCount() { return unseen.length; },
    get newestArrivalId() { return unseen[0]?.id ?? null; },
    acknowledgeArrivals,
    showNewestArrival(): void {
      pendingArrivalId = unseen[0]?.id ?? null;
      acknowledgeArrivals();
    },
    get pendingArrivalId() { return pendingArrivalId; },
    consumePendingArrival(): void { pendingArrivalId = null; },
    attachFocusProbe(probe: () => boolean): () => void {
      focusProbe = probe;
      return () => {
        if (focusProbe === probe) focusProbe = null;
      };
    },
    get anchor() { return anchor; },
    setAnchor(next: ConversationFeedAnchor | null): void { anchor = next; },
    beginExplicitRead(): void { explicitReads += 1; },
    endExplicitRead(answered: boolean): void {
      sync();
      explicitReads = Math.max(0, explicitReads - 1);
      if (answered) acknowledgeArrivals();
    },
    joinPostedComment(commentId: string): void {
      sync();
      if (order.length === 0) return;
      const mine = untrack(() => host.source()).find(({ item }) => item.kind === 'thread'
        && !item.thread.isResolvable && item.thread.comments[0]?.id === commentId);
      if (!mine || !order.includes(mine.item.id)) return;
      const id = mine.item.id;
      if (!seen.has(id)) seen = new Set([...seen, id]);
      pendingArrivalId = id;
    },
    sync,
    threadExpanded,
    toggleThread(threadId: string): void {
      expandOverrides.set(threadId, !threadExpanded(threadId));
    },
    expandThread(threadId: string): void {
      if (!threadExpanded(threadId)) expandOverrides.set(threadId, true);
    },
    reset,
    dispose(): void {
      disposeWatch();
    },
  };
}
