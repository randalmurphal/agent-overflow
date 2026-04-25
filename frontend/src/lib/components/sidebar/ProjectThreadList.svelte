<script lang="ts">
  // Nested thread list for a single project. Builds the discussion tree
  // (depth ≤ 2) once per render so child status / activity bubbles into
  // each parent row, then truncates to THREAD_PREVIEW_LIMIT (with the
  // active thread always pinned in if it would otherwise sit below the
  // fold), and finally flattens to a flat list of visible nodes honoring
  // the per-thread expand/collapse state. Indent comes from the node's
  // depth (rendered via ThreadRow's `indent` prop). We deliberately do
  // NOT virtualize — at 50 threads the flat render is 60fps and the
  // tree-level sort already pushes off-screen rows below the fold.

  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    collapseThreadList,
    expandThreadList,
    getExpandedDiscussions,
    isThreadListExpanded,
    setExpandedDiscussions,
    toggleDiscussion,
  } from '../../stores/sidebar.svelte';
  import {
    isThreadSelected,
    toggleThreadSelection,
  } from '../../stores/threadFilter.svelte';
  import { getThreadStatus } from '../../stores/threadStatuses.svelte';
  import { autoAnimate } from '../../utils/autoAnimate';
  import ThreadRow from './ThreadRow.svelte';
  import {
    buildSidebarThreadTree,
    flattenSidebarThreadTree,
    previewSidebarThreads,
    rollupDisplayStatus,
    syncExpandedTreeForActiveThread,
    THREAD_PREVIEW_LIMIT,
  } from '../../utils/sidebarTree';

  interface Props {
    projectId: string;
    threads: Thread[];
    pane: ThreadPane;
  }

  let { projectId, threads, pane }: Props = $props();

  // Tree is built per-render: cheap (small N) and lets us reactively
  // pick up live-status changes through getThreadStatus.
  let tree = $derived(
    buildSidebarThreadTree({
      threads,
      liveStatusOf: (id) => getThreadStatus(id),
    }),
  );

  let listExpanded = $derived(isThreadListExpanded(projectId));

  // Truncation operates on top-level nodes only — discussion children
  // are nested under their parent and don't compete for preview slots.
  let preview = $derived.by(() => {
    if (listExpanded) return { visibleNodes: tree, hiddenNodes: [] };
    return previewSidebarThreads({ nodes: tree, activeThreadId: pane.threadId ?? null });
  });

  let hiddenStatus = $derived(rollupDisplayStatus(preview.hiddenNodes));

  // Auto-expand the chain of ancestors leading to the active thread so
  // a freshly-switched discussion participant shows up without a manual
  // chevron click. Drops expanded ids that no longer point at expandable
  // nodes (a child thread was deleted, parent is now a leaf).
  $effect(() => {
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: getExpandedDiscussions(),
      activeThreadId: pane.threadId ?? null,
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
    expandThreadList(projectId);
  }

  function handleShowLess(e: MouseEvent): void {
    e.stopPropagation();
    collapseThreadList(projectId);
  }

  /**
   * Multi-select row click handler. ⌘/⌃-click toggles a thread in the
   * selection; shift-click is folded into toggle for now (range-select
   * needs an anchor that we haven't wired yet — see comment below). A
   * plain click without a modifier falls through to thread switch via
   * the row's own switchThread; ProjectThreadList's job here is just to
   * decide whether to suppress that switch when a modifier is held.
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

</script>

{#if threads.length === 0}
  <p
    class="ml-6 mr-2 my-1 text-[11px] text-fg-hint italic select-none"
    data-testid="project-thread-list-empty"
  >
    No Threads Yet
  </p>
{:else}
  <div
    class="flex flex-col gap-px px-1"
    role="list"
    aria-label="Project Threads"
    data-testid="project-thread-list"
    use:autoAnimate
  >
    {#each visibleNodes as node (node.thread.id)}
      <div role="listitem">
        <ThreadRow
          thread={node.thread}
          {pane}
          indent={node.depth + 1}
          hasChildren={node.isExpandable}
          expanded={node.isExpanded}
          onToggleExpand={() => toggleDiscussion(node.thread.id)}
          selected={isThreadSelected(node.thread.id)}
          onSelectClick={(modifier) => handleSelectClick(node.thread.id, modifier)}
        />
      </div>
    {/each}

    {#if preview.hiddenNodes.length > 0 && !listExpanded}
      <button
        type="button"
        onclick={handleShowMore}
        data-testid="project-thread-list-show-more"
        class="group/more flex items-center gap-1.5 h-6 ml-3 mr-1 px-2 rounded-[var(--radius-field)] text-[10px] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
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
        <span>Show {preview.hiddenNodes.length} More</span>
      </button>
    {/if}

    {#if listExpanded && tree.length > THREAD_PREVIEW_LIMIT}
      <button
        type="button"
        onclick={handleShowLess}
        data-testid="project-thread-list-show-less"
        class="flex items-center h-6 ml-3 mr-1 px-2 rounded-[var(--radius-field)] text-[10px] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      >
        Show Less
      </button>
    {/if}
  </div>
{/if}
