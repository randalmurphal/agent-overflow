<script lang="ts">
  // Find for a pane that renders a thread timeline: registers the thread
  // find provider (stores/threadFind.svelte.ts) for its layout pane while
  // mounted and the thread's owner serves the find contract, and renders
  // that pane's find bar. The host mounts it directly under the pane header.
  // DOM work lives here: the reading position, the on-screen test and the
  // highlights over the timeline's mounted rows.
  import { untrack } from 'svelte';
  import PaneFindBar from '../panes/PaneFindBar.svelte';
  import { getPaneFind, registerPaneFindProvider } from '../../stores/paneFind.svelte';
  import { createThreadFind, threadFindScopeKey, type ThreadFindScope } from '../../stores/threadFind.svelte';
  import { threadFindBackend } from '../../stores/timelineSelectionSupport';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadScrollSnapshot } from '../../utils/threadScrollSnapshots';
  import { createFindHighlighter, findHighlightName, type FindHighlighter } from '../../utils/findHighlights';

  interface Props {
    /** The layout pane the find belongs to. */
    paneId: string;
    title: string;
    /** The pane (or scoped view) whose MessageTimeline renders the rows. */
    timeline: ThreadPane | null;
    scope: { threadId: string; scopeRootId?: string } | null;
    /** Element containing the timeline's scroll container. */
    root: HTMLElement | null;
    /** Completes the placeholder: "Find in {noun}". */
    noun?: string;
  }

  let { paneId, title, timeline, scope, root, noun = 'thread' }: Props = $props();

  const ROW_SELECTOR = '[data-item-id]';

  function scroller(): HTMLElement | null {
    return root?.querySelector<HTMLElement>('[data-testid="message-timeline-scroll"]') ?? null;
  }

  function rowElement(container: HTMLElement, itemId: string): HTMLElement | null {
    return container.querySelector<HTMLElement>(`[data-item-id="${CSS.escape(itemId)}"]`);
  }

  let highlighter: FindHighlighter | null = null;
  let highlighterRoot: HTMLElement | null = null;
  let paintedQuery = '';

  function releaseHighlighter(): void {
    highlighter?.dispose();
    highlighter = null;
    highlighterRoot = null;
    paintedQuery = '';
  }

  // The transcript a read is asked for, with its owner and the timeline's
  // generation, so an answer from before a move or a reload is dropped.
  let backend = $derived(scope ? threadFindBackend(scope.threadId) : null);
  let findScope = $derived.by((): ThreadFindScope | null =>
    scope && timeline && backend !== null
      ? { ...scope, backend, generation: timeline.switchGeneration }
      : null);

  const find = createThreadFind({
    paneId: () => paneId,
    scope: () => findScope,
    pane: () => timeline,
    // The position the timeline last recorded through its virtualizer
    // handle: at the bottom nothing lies below, so the last hit is current.
    // A timeline that recorded nothing yet sits where it opened, at its tail.
    readingPosition: () => {
      if (!timeline) return null;
      const key = timeline.scrollStateKey;
      const snapshot = key ? getThreadScrollSnapshot(key) : undefined;
      const atTail = snapshot ? snapshot.kind === 'bottom' : !timeline.hasMoreNewer;
      if (atTail) return { turnIndex: Number.MAX_SAFE_INTEGER, itemIndex: 0 };
      if (snapshot?.kind !== 'anchor') return null;
      const item = timeline.getItemById(snapshot.itemId);
      return item ? { turnIndex: item.turnIndex, itemIndex: item.itemIndex } : null;
    },
    isOnScreen: (itemId) => {
      const container = scroller();
      const row = container ? rowElement(container, itemId) : null;
      if (!container || !row) return false;
      const view = container.getBoundingClientRect();
      const rect = row.getBoundingClientRect();
      return rect.height > 0 && rect.bottom > view.top && rect.top < view.bottom;
    },
    paint: (query, currentItemId) => {
      if (!query) {
        releaseHighlighter();
        return;
      }
      const container = scroller();
      if (!container) return;
      if (highlighterRoot !== container) {
        releaseHighlighter();
        highlighter = createFindHighlighter({
          root: container,
          name: findHighlightName(paneId),
          rowSelector: ROW_SELECTOR,
          rowKeyAttribute: 'data-item-id',
        });
        highlighterRoot = container;
      }
      if (query !== paintedQuery) {
        paintedQuery = query;
        highlighter!.setQuery(query);
      }
      highlighter!.setCurrent(currentItemId ? { row: currentItemId } : null);
    },
  }, () => noun);

  // A host that predates the find contract answers the main search with
  // agent rows included, so its threads get no find: mod+f falls through.
  let supported = $derived(backend !== null);
  $effect(() => {
    const id = paneId;
    if (!supported) return;
    const release = untrack(() => registerPaneFindProvider(id, find));
    return () => {
      release();
      releaseHighlighter();
    };
  });

  // Another transcript, owner or generation (thread switch, agent scope,
  // conversation move, reload) drops the hits and searches again.
  let scopeKey = untrack(() => threadFindScopeKey(findScope));
  $effect(() => {
    const key = threadFindScopeKey(findScope);
    if (key === scopeKey) return;
    scopeKey = key;
    untrack(() => {
      if (getPaneFind(paneId)?.open) find.rescope();
    });
  });

  // Rows the transcript gains or whose text changes (streaming included)
  // can change the hits: re-read, rate-bounded. Tracked only while open,
  // and only then does the timeline count events for rows it does not hold.
  $effect(() => {
    if (getPaneFind(paneId)?.open !== true || !timeline) return;
    const surface = timeline;
    return untrack(() => surface.watchSearchableEvents());
  });
  let seen: { timeline: ThreadPane; revision: number } | null = null;
  $effect(() => {
    if (getPaneFind(paneId)?.open !== true || !timeline) {
      seen = null;
      return;
    }
    const revision = timeline.searchRevision;
    const changed = seen !== null && seen.timeline === timeline && seen.revision !== revision;
    seen = { timeline, revision };
    if (changed) untrack(() => find.refresh());
  });
</script>

<PaneFindBar {paneId} {title} fallbackFocus={scroller} />
