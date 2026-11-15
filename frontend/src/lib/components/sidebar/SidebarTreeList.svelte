<script lang="ts" module>
  /**
   * The drop targets a project's list puts on its rows (ProjectThreadList).
   * A group, its member rows and its show-more take a thread into the
   * group.
   */
  export interface SidebarTreeDrops {
    memberDragOver(e: DragEvent, groupId: string): void;
    memberDragLeave(e: DragEvent, groupId: string): void;
    memberDrop(e: DragEvent, groupId: string): void;
    /** Whether the group row lights as taking the thread dragged over it. */
    groupLit(groupId: string): boolean;
    /** The group row reports a drag entering or leaving it. */
    groupTargetChange(groupId: string, active: boolean, accepts: boolean): void;
  }
</script>

<script lang="ts">
  // The rows of one sidebar tree: a project's list, or the Assistants
  // section's rows. Builds the sorted rows once per render, with each
  // group's member status and activity bubbled into its row, then truncates
  // to the list's visible limit (with open threads always floated in if
  // they would otherwise sit below the fold), and finally flattens to one
  // list honoring each group's collapse state. Every thread renders flat:
  // at the top level or as a group member (ThreadRow's `indent` comes from
  // the node's depth).
  // We deliberately do NOT virtualize: at 50 threads the flat render is
  // 60fps and the sort already pushes off-screen rows below the fold.
  //
  // The caller owns the list element these rows render in, and what only
  // its list has: a project's empty state, pending forks and drop targets.
  import type { Thread, ThreadGroup } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    collapseThreadList,
    getContainerVisibleLimit,
    getCollapsedGroups,
    getThreadListVisibleLimit,
    isGroupExpanded,
    isThreadListExpanded,
    setContainerVisibleLimit,
    setThreadListVisibleLimit,
  } from '../../stores/sidebar.svelte';
  import { isThreadSelected, toggleThreadSelection } from '../../stores/threadFilter.svelte';
  import { getEffectiveThreadStatus, getThreadWaitingOn } from '../../stores/threadStatuses.svelte';
  import { unreachableThreadMachineName } from '../../stores/attachedBackends.svelte';
  import { getThreadLiveActivityAt } from '../../stores/threads.svelte';
  import { openThreadIds } from '../../stores/panes.svelte';
  import { sidebarFlip, sidebarEnter, sidebarExit } from '../../utils/sidebarAnimate';
  import { buildSidebarThreadTree } from '../../utils/sidebarTree';
  import {
    flattenSidebarThreadTree,
    nextContainerRevealLimit,
    nextSidebarThreadRevealLimit,
    previewSidebarThreads,
    rollupDisplayStatus,
    sameSidebarVisibleNodes,
    sameThreadStatusPill,
    sidebarVisibleNodeId,
    type SidebarContainerMoreNode,
    type SidebarTreeVisibleNode,
  } from '../../utils/sidebarTreeView';
  import { THREAD_PREVIEW_LIMIT, THREAD_REVEAL_INCREMENT } from '../../utils/sidebarThreadLimits';
  import { GROUP_RAIL_LEFT_PX } from '../../utils/sidebarRowMetrics';
  import ContainerShowMore from './ContainerShowMore.svelte';
  import ThreadGroupRow from './ThreadGroupRow.svelte';
  import ThreadRow from './ThreadRow.svelte';

  interface Props {
    /** The list's threads; a fork still being prepared is the caller's pending row, not one of these. */
    threads: readonly Thread[];
    /** The project's groups, already search-filtered by ProjectsSection. */
    groups?: readonly ThreadGroup[];
    /** The key the list's show-more paging persists under (stores/sidebar.svelte.ts). */
    listKey: string;
    pane: ThreadPane | null;
    /** An assistant row's third line, the projects its assistant serves; none when absent. */
    agentProjects?: (agent: Thread) => string;
    /** The project list's drop targets; without them a row takes no drop. */
    drops?: SidebarTreeDrops;
  }

  let { threads, groups = [], listKey, pane, agentProjects, drops }: Props = $props();

  // Tree is built per-render: cheap (small N) and lets us reactively
  // pick up effective live-status changes from the status store and
  // live-activity ordering from the per-thread activity boxes. Streaming
  // beats DO wake this derived (activity is sort input); the identity
  // cutoff on visibleNodes below is what keeps those beats from
  // reaching the DOM.
  let tree = $derived(
    buildSidebarThreadTree({
      threads: threads.filter((thread) => !thread.forkPreparing),
      groups,
      statusOf: (thread) => getEffectiveThreadStatus(thread),
      waitingOnOf: getThreadWaitingOn,
      unreachableMachineOf: (thread) => unreachableThreadMachineName(thread.id, thread.projectId),
      activityOf: (thread) => getThreadLiveActivityAt(thread),
    }),
  );

  // Threads mounted in any pane never hide behind the cut. Re-minted only
  // when a pane opens, closes, or swaps thread.
  let openIds = $derived(openThreadIds());
  let listExpanded = $derived(isThreadListExpanded(listKey));
  let visibleLimit = $derived(getThreadListVisibleLimit(listKey));

  // Truncation operates on top-level nodes only: group members sit under
  // their group and don't compete for preview slots.
  let preview = $derived.by(() => {
    return previewSidebarThreads({
      nodes: tree,
      openThreadIds: openIds,
      limit: visibleLimit,
    });
  });

  let hiddenThreadCount = $derived(preview.hiddenNodes.length);
  let nextRevealCount = $derived(Math.min(THREAD_REVEAL_INCREMENT, hiddenThreadCount));

  // Identity cutoff: the rollup object is minted per tree build, so
  // return the previous one when its content is unchanged; otherwise
  // every streaming beat re-renders the show-more footer.
  let prevHiddenStatus: ReturnType<typeof rollupDisplayStatus> = null;
  let hiddenStatus = $derived.by(() => {
    const next = rollupDisplayStatus(preview.hiddenNodes);
    if (
      prevHiddenStatus !== null && next !== null
      && prevHiddenStatus.liveStatus === next.liveStatus
      && sameThreadStatusPill(prevHiddenStatus.pill, next.pill)
    ) {
      return prevHiddenStatus;
    }
    prevHiddenStatus = next;
    return next;
  });

  // Identity cutoff: return the PREVIOUS array when nothing the rows
  // render has changed, so svelte's derived cutoff stops the animated
  // each-block from reconciling. This is load-bearing for streaming
  // performance: without it every item-event flush re-ran the FLIP
  // measure pass (a forced synchronous layout per beat). See
  // sameSidebarVisibleNodes.
  let prevVisibleNodes: ReturnType<typeof flattenSidebarThreadTree> = [];
  let visibleNodes = $derived.by(() => {
    const next = flattenSidebarThreadTree({
      nodes: preview.visibleNodes,
      collapsedGroupIds: getCollapsedGroups(),
      activeThreadId: pane?.threadId ?? null,
      openThreadIds: openIds,
      memberLimitOf: getContainerVisibleLimit,
    });
    if (sameSidebarVisibleNodes(prevVisibleNodes, next)) return prevVisibleNodes;
    prevVisibleNodes = next;
    return next;
  });

  // A group's show-more: the members its bound cuts.
  function handleContainerReveal(more: SidebarContainerMoreNode): void {
    for (const node of tree) {
      if (node.kind !== 'group' || node.group.id !== more.containerKey) continue;
      const key = node.group.id;
      setContainerVisibleLimit(key, nextContainerRevealLimit(node.children, openIds, getContainerVisibleLimit(key)));
      return;
    }
  }

  function handleShowMore(e: MouseEvent): void {
    e.stopPropagation();
    const nextLimit = nextSidebarThreadRevealLimit({
      nodes: tree,
      openThreadIds: openIds,
      currentLimit: visibleLimit,
      revealCount: THREAD_REVEAL_INCREMENT,
    });
    setThreadListVisibleLimit(listKey, nextLimit);
  }

  function handleShowLess(e: MouseEvent): void {
    e.stopPropagation();
    collapseThreadList(listKey);
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

  // A row's wrapper hands a drag to the group it renders inside.
  function dragOver(e: DragEvent, node: SidebarTreeVisibleNode): void {
    if (drops && node.ownerGroupId) drops.memberDragOver(e, node.ownerGroupId);
  }

  function dragLeave(e: DragEvent, node: SidebarTreeVisibleNode): void {
    if (drops && node.ownerGroupId) drops.memberDragLeave(e, node.ownerGroupId);
  }

  function drop(e: DragEvent, node: SidebarTreeVisibleNode): void {
    if (drops && node.ownerGroupId) drops.memberDrop(e, node.ownerGroupId);
  }
</script>

{#each visibleNodes as node (sidebarVisibleNodeId(node))}
  <!-- role="presentation" keeps the animated wrapper transparent to the
       list > listitem structure now that the divider shares it. -->
  <div role="presentation" animate:sidebarFlip in:sidebarEnter out:sidebarExit>
    {#if node.startsSection}
      <div
        role="separator"
        aria-hidden="true"
        data-testid="thread-section-divider"
        class="mx-2 my-1 border-t border-border-subtle/60"
      ></div>
    {/if}
    {#if node.kind === 'container-more'}
      <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
      <div
        role="listitem"
        class="border-l border-border-subtle/60 pb-px -mb-px"
        style:margin-left="{GROUP_RAIL_LEFT_PX}px"
        data-group-member=""
        ondragover={(e) => dragOver(e, node)}
        ondragleave={(e) => dragLeave(e, node)}
        ondrop={(e) => drop(e, node)}
      >
        <ContainerShowMore {node} onReveal={() => handleContainerReveal(node)} />
      </div>
    {:else if node.kind === 'group'}
      <div role="listitem">
        <ThreadGroupRow
          group={node.group}
          {pane}
          expanded={isGroupExpanded(node.group.id)}
          memberThreadIds={node.children.map((member) => member.thread.id)}
          status={node.displayStatus === null ? null : { liveStatus: node.displayLiveStatus, pill: node.displayStatus }}
          dropActive={drops?.groupLit(node.group.id) ?? false}
          onDropTargetChange={(active, accepts) => drops?.groupTargetChange(node.group.id, active, accepts)}
        />
      </div>
    {:else}
      <!--
        A group member sits behind its own rail, dropped from the centre
        of the group row's chevron (GROUP_RAIL_LEFT_PX), so "inside the
        group" is a line and not an
        indent the eye has to measure. Rows are siblings in one animated list (no
        wrapper per group), so each member draws its own segment; the
        1px pad-and-pull-back carries the border across the list's
        gap-px so the segments join into one line.
      -->
      <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
      <div
        role="listitem"
        class={node.ownerGroupId ? 'border-l border-border-subtle/60 pb-px -mb-px' : ''}
        style:margin-left={node.ownerGroupId ? `${GROUP_RAIL_LEFT_PX}px` : null}
        data-group-member={node.ownerGroupId ? '' : null}
        ondragover={(e) => dragOver(e, node)}
        ondragleave={(e) => dragLeave(e, node)}
        ondrop={(e) => drop(e, node)}
      >
        <ThreadRow
          thread={node.thread}
          {pane}
          indent={node.depth + 1}
          inGroup={node.ownerGroupId !== null}
          agentProjects={node.thread.agentId ? (agentProjects?.(node.thread) ?? null) : null}
          selected={isThreadSelected(node.thread.id)}
          onSelectClick={(modifier) => handleSelectClick(node.thread.id, modifier)}
        />
      </div>
    {/if}
  </div>
{/each}

{#if hiddenThreadCount > 0 || (listExpanded && tree.length > THREAD_PREVIEW_LIMIT)}
  <div class="flex items-center gap-1 mr-1">
    {#if hiddenThreadCount > 0}
      <button
        type="button"
        onclick={handleShowMore}
        data-testid="project-thread-list-show-more"
        class="group/more flex items-center gap-1.5 h-6 compact:h-9 px-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      >
        {#if hiddenStatus}
          <span
            class="w-1.5 h-1.5 rounded-full shrink-0 {hiddenStatus.pill.dotClass} {hiddenStatus.pill.pulse ? 'animate-pulse' : ''}"
            aria-hidden="true"
            data-testid="project-thread-list-hidden-status"
            data-status={hiddenStatus.liveStatus}
          ></span>
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
        class="flex items-center h-6 compact:h-9 px-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      >
        Show Less
      </button>
    {/if}
  </div>
{/if}
