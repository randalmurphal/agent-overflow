<script lang="ts">
  // Nested thread list for a single project. Builds the discussion tree
  // (depth ≤ 2) once per render so child status / activity bubbles into
  // each parent row, then truncates to the project-specific visible
  // limit (with the active thread always pinned in if it would otherwise
  // sit below the fold), and finally flattens to a flat list honoring
  // the per-thread expand/collapse state. Indent comes from the node's
  // depth (rendered via ThreadRow's `indent` prop). We deliberately do
  // NOT virtualize — at 50 threads the flat render is 60fps and the
  // tree-level sort already pushes off-screen rows below the fold.

  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    shouldOpenProjectThreadInNewPane,
    type ProjectNewThreadHandler,
  } from './projectNewThread';
  import {
    collapseThreadList,
    getExpandedDiscussions,
    getThreadListVisibleLimit,
    isThreadListExpanded,
    setThreadListVisibleLimit,
    setExpandedDiscussions,
    toggleDiscussion,
  } from '../../stores/sidebar.svelte';
  import {
    isThreadSelected,
    toggleThreadSelection,
  } from '../../stores/threadFilter.svelte';
  import {
    getEffectiveThreadStatus,
  } from '../../stores/threadStatuses.svelte';
  import { flip } from 'svelte/animate';
  import { SIDEBAR_FLIP, sidebarEnter, sidebarExit } from '../../utils/sidebarAnimate';
  import Plus from '@lucide/svelte/icons/plus';
  import Icon from '../primitives/Icon.svelte';
  import ThreadRow from './ThreadRow.svelte';
  import {
    buildSidebarThreadTree,
    flattenSidebarThreadTree,
    nextSidebarThreadRevealLimit,
    previewSidebarThreads,
    rollupDisplayStatus,
    syncExpandedTreeForActiveThread,
  } from '../../utils/sidebarTree';
  import { THREAD_PREVIEW_LIMIT, THREAD_REVEAL_INCREMENT } from '../../utils/sidebarThreadLimits';

  interface Props {
    projectId: string;
    threads: Thread[];
    pane: ThreadPane | null;
    /** Click handler for the empty-state "+ New Thread" button. */
    onNewThread?: ProjectNewThreadHandler;
  }

  let { projectId, threads, pane, onNewThread }: Props = $props();
  let lastNewThreadContextMenuAt = 0;

  // Tree is built per-render: cheap (small N) and lets us reactively
  // pick up effective live-status changes from the status store.
  let tree = $derived(
    buildSidebarThreadTree({
      threads,
      statusOf: (thread) => getEffectiveThreadStatus(thread),
    }),
  );

  let listExpanded = $derived(isThreadListExpanded(projectId));
  let visibleLimit = $derived(getThreadListVisibleLimit(projectId));

  // Truncation operates on top-level nodes only — discussion children
  // are nested under their parent and don't compete for preview slots.
  let preview = $derived.by(() => {
    return previewSidebarThreads({
      nodes: tree,
      activeThreadId: pane?.threadId ?? null,
      limit: visibleLimit,
    });
  });

  let hiddenThreadCount = $derived(preview.hiddenNodes.length);
  let nextRevealCount = $derived(Math.min(THREAD_REVEAL_INCREMENT, hiddenThreadCount));

  let hiddenStatus = $derived(rollupDisplayStatus(preview.hiddenNodes));

  // Auto-expand the chain of ancestors leading to the active thread so
  // a freshly-switched discussion participant shows up without a manual
  // chevron click. Drops expanded ids that no longer point at expandable
  // nodes (a child thread was deleted, parent is now a leaf).
  $effect(() => {
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: getExpandedDiscussions(),
      activeThreadId: pane?.threadId ?? null,
    });
    setExpandedDiscussions(next);
  });

  let visibleNodes = $derived(
    flattenSidebarThreadTree({
      nodes: preview.visibleNodes,
      expandedThreadIds: getExpandedDiscussions(),
    }),
  );

  function handleShowMore(e: MouseEvent): void {
    e.stopPropagation();
    const nextLimit = nextSidebarThreadRevealLimit({
      nodes: tree,
      activeThreadId: pane?.threadId ?? null,
      currentLimit: visibleLimit,
      revealCount: THREAD_REVEAL_INCREMENT,
    });
    setThreadListVisibleLimit(projectId, nextLimit);
  }

  function handleShowLess(e: MouseEvent): void {
    e.stopPropagation();
    collapseThreadList(projectId);
  }

  /**
   * Multi-select row click handler. Shift-click is folded into toggle for
   * now; Cmd/Ctrl-click is reserved for opening or focusing a pane. A plain
   * click falls through to thread switch via the row's own switchThread.
   */
  function handleSelectClick(
    threadId: string,
    modifier: 'toggle' | 'range' | 'single' | null,
  ): boolean {
    if (modifier === 'toggle' || modifier === 'range') {
      toggleThreadSelection(threadId);
      // Suppress the switchThread fallback so the user's selection click
      // doesn't ALSO swap the active pane.
      return true;
    }
    return false;
  }

  function handleEmptyNewThreadClick(e: MouseEvent): void {
    if (Date.now() - lastNewThreadContextMenuAt < 500) return;
    onNewThread?.(projectId, {
      openInNewPane: shouldOpenProjectThreadInNewPane(e),
    });
  }

  function handleEmptyNewThreadContextMenu(e: MouseEvent): void {
    if (!shouldOpenProjectThreadInNewPane(e)) return;
    e.preventDefault();
    e.stopPropagation();
    lastNewThreadContextMenuAt = Date.now();
    onNewThread?.(projectId, { openInNewPane: true });
  }

</script>

{#if threads.length === 0}
  <button
    type="button"
    onclick={handleEmptyNewThreadClick}
    oncontextmenu={handleEmptyNewThreadContextMenu}
    data-testid="project-thread-list-empty"
    class="ml-4 mr-2 my-1 inline-flex items-center gap-1 rounded-[var(--radius-field)] px-2 py-1 text-[0.6875rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
  >
    <Icon icon={Plus} size={11} strokeWidth={2.2} class="opacity-80" />
    <span>New Thread</span>
  </button>
{:else}
  <div
    class="flex flex-col gap-px ml-2 border-l border-border-subtle/60"
    role="list"
    aria-label="Project Threads"
    data-testid="project-thread-list"
  >
    {#each visibleNodes as node (node.thread.id)}
      <div role="listitem" animate:flip={SIDEBAR_FLIP} in:sidebarEnter out:sidebarExit>
        <ThreadRow
          thread={node.thread}
          {pane}
          indent={node.depth + 1}
          hasChildren={node.isExpandable}
          expanded={node.isExpanded}
          displayLiveStatus={node.displayLiveStatus}
          displayStatus={node.displayStatus}
          onToggleExpand={() => toggleDiscussion(node.thread.id)}
          selected={isThreadSelected(node.thread.id)}
          onSelectClick={(modifier) => handleSelectClick(node.thread.id, modifier)}
        />
      </div>
    {/each}

    {#if hiddenThreadCount > 0 || (listExpanded && tree.length > THREAD_PREVIEW_LIMIT)}
      <div class="flex items-center gap-1 mr-1">
        {#if hiddenThreadCount > 0}
          <button
            type="button"
            onclick={handleShowMore}
            data-testid="project-thread-list-show-more"
            class="group/more flex items-center gap-1.5 h-6 pl-6 pr-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          >
            {#if hiddenStatus}
              <span
                class="w-1.5 h-1.5 rounded-full shrink-0 {hiddenStatus.pill.dotClass} {hiddenStatus.pill.pulse ? 'animate-pulse' : ''}"
                aria-hidden="true"
                data-testid="project-thread-list-hidden-status"
                data-status={hiddenStatus.liveStatus}
              ></span>
              <span class="font-medium {hiddenStatus.pill.labelClass}">{hiddenStatus.pill.label}</span>
              <span aria-hidden="true">·</span>
            {/if}
            <span>
              Show {nextRevealCount} More{hiddenThreadCount > THREAD_REVEAL_INCREMENT ? ` (${hiddenThreadCount})` : ''}
            </span>
          </button>
        {/if}

        {#if listExpanded && tree.length > THREAD_PREVIEW_LIMIT}
          <button
            type="button"
            onclick={handleShowLess}
            data-testid="project-thread-list-show-less"
            class="flex items-center h-6 px-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          >
            Show Less
          </button>
        {/if}
      </div>
    {/if}
  </div>
{/if}
