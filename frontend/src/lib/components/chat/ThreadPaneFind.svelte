<script lang="ts">
  // Find for a pane that renders a thread timeline: registers the thread
  // find provider (stores/threadFind.svelte.ts) for its layout pane while
  // mounted and renders that pane's find bar. The host mounts it directly
  // under the pane header. DOM work lives here: the reading position, the
  // on-screen test and the highlights over the timeline's mounted rows.
  import { untrack } from 'svelte';
  import PaneFindBar from '../panes/PaneFindBar.svelte';
  import { getPaneFind, registerPaneFindProvider } from '../../stores/paneFind.svelte';
  import { createThreadFind, type ThreadFindScope } from '../../stores/threadFind.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadScrollSnapshot } from '../../utils/threadScrollSnapshots';
  import { createFindHighlighter, findHighlightName, type FindHighlighter } from '../../utils/findHighlights';

  interface Props {
    /** The layout pane the find belongs to. */
    paneId: string;
    title: string;
    /** The pane (or scoped view) whose MessageTimeline renders the rows. */
    timeline: ThreadPane | null;
    scope: ThreadFindScope | null;
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

  const find = createThreadFind({
    scope: () => scope,
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

  $effect(() => {
    const id = paneId;
    const release = untrack(() => registerPaneFindProvider(id, find));
    return () => {
      release();
      releaseHighlighter();
    };
  });

  // A new transcript (thread switch, another agent scope) is a new search
  // for the same query.
  let scopeKey = untrack(() => (scope ? `${scope.threadId}:${scope.scopeRootId ?? ''}` : ''));
  $effect(() => {
    const key = scope ? `${scope.threadId}:${scope.scopeRootId ?? ''}` : '';
    if (key === scopeKey) return;
    scopeKey = key;
    untrack(() => {
      const state = getPaneFind(paneId);
      if (state?.open && state.query) find.search(state.query);
    });
  });

  // Rows the transcript gains or changes can add hits: re-read, rate-bounded.
  $effect(() => {
    void timeline?.timelineRevision;
    untrack(() => {
      if (getPaneFind(paneId)?.open) find.refresh();
    });
  });
</script>

<PaneFindBar {paneId} {title} fallbackFocus={scroller} />
