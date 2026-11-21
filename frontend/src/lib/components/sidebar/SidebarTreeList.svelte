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
    ASSISTANTS_LIST_KEY,
    collapseThreadList,
    getCollapsedGroups,
    getThreadListVisibleLimit,
    isGroupExpanded,
    setThreadListVisibleLimit,
    type SidebarTopLevelList,
  } from '../../stores/sidebar.svelte';
  import { resetEntryListLimit, setEntryListLimit } from '../../stores/sidebarEntryContinuity.svelte';
  import { isThreadSelected, toggleThreadSelection } from '../../stores/threadFilter.svelte';
  import { getEffectiveThreadStatus, getThreadWaitingOn } from '../../stores/threadStatuses.svelte';
  import { unreachableThreadMachineName } from '../../stores/attachedBackends.svelte';
  import { getThreadLiveActivityAt } from '../../stores/threads.svelte';
  import { openThreadIds } from '../../stores/panes.svelte';
  import { sidebarFlip, sidebarEnter, sidebarExit } from '../../utils/sidebarAnimate';
  import { buildSidebarThreadTree, type SidebarGroupTreeNode, type SidebarTreeNode } from '../../utils/sidebarTree';
  import {
    flattenSidebarThreadTree,
    nextSidebarThreadRevealLimit,
    previewSidebarThreads,
    rollupDisplayStatus,
    sameSidebarVisibleRows,
    sameThreadStatusPill,
    sidebarVisibleRowKey,
  } from '../../utils/sidebarTreeView';
  import { THREAD_REVEAL_INCREMENT } from '../../utils/sidebarThreadLimits';
  import { GROUP_RAIL_LEFT_PX } from '../../utils/sidebarRowMetrics';
  import ThreadGroupRow from './ThreadGroupRow.svelte';
  import ThreadListMoreFooter from './ThreadListMoreFooter.svelte';
  import ThreadRow from './ThreadRow.svelte';

  interface Props {
    /** The list's threads; a fork still being prepared is the caller's pending row, not one of these. */
    threads: readonly Thread[];
    /** The project's groups, already search-filtered by ProjectsSection. */
    groups?: readonly ThreadGroup[];
    /** What the list's top level pages under: a project entry, or the
     *  Assistants section. The two page in records of their own
     *  (stores/sidebar.svelte.ts). */
    list: SidebarTopLevelList;
    pane: ThreadPane | null;
    /** An assistant row's third line, the projects its assistant serves; none when absent. */
    agentProjects?: (agent: Thread) => string;
    /** The project list's drop targets; without them a row takes no drop. */
    drops?: SidebarTreeDrops;
  }

  let { threads, groups = [], list, pane, agentProjects, drops }: Props = $props();

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
  let visibleLimit = $derived(list.kind === 'project'
    ? list.limit
    : getThreadListVisibleLimit(ASSISTANTS_LIST_KEY));

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
  // sameSidebarVisibleRows.
  let prevVisibleNodes: ReturnType<typeof flattenSidebarThreadTree> = [];
  let visibleNodes = $derived.by(() => {
    const next = flattenSidebarThreadTree({
      nodes: preview.visibleNodes,
      collapsedGroupIds: getCollapsedGroups(),
      activeThreadId: pane?.threadId ?? null,
      openThreadIds: openIds,
      groupVisibleLimit: getThreadListVisibleLimit,
    });
    if (sameSidebarVisibleRows(prevVisibleNodes, next)) return prevVisibleNodes;
    prevVisibleNodes = next;
    return next;
  });

  /** The next step of a list, from the rows it holds. */
  function nextLimit(nodes: readonly SidebarTreeNode[], currentLimit: number): number {
    return nextSidebarThreadRevealLimit({
      nodes,
      openThreadIds: openIds,
      currentLimit,
      revealCount: THREAD_REVEAL_INCREMENT,
    });
  }

  /** Reveal the next step of the list's own top level, in its own record. */
  function showMoreTopLevel(): void {
    const limit = nextLimit(tree, visibleLimit);
    if (list.kind === 'project') setEntryListLimit(list.entryKey, limit);
    else setThreadListVisibleLimit(ASSISTANTS_LIST_KEY, limit);
  }

  function collapseTopLevel(): void {
    if (list.kind === 'project') resetEntryListLimit(list.entryKey);
    else collapseThreadList(ASSISTANTS_LIST_KEY);
  }

  /** A group's members page by group id, whatever list they are in. */
  function showMoreGroup(groupId: string): void {
    const group = tree.find((node): node is SidebarGroupTreeNode => node.kind === 'group' && node.group.id === groupId);
    if (!group) throw new Error(`Show more: group ${groupId} is not in this list`);
    setThreadListVisibleLimit(groupId, nextLimit(group.children, getThreadListVisibleLimit(groupId)));
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
  function dragOver(e: DragEvent, groupId: string | null): void {
    if (drops && groupId) drops.memberDragOver(e, groupId);
  }

  function dragLeave(e: DragEvent, groupId: string | null): void {
    if (drops && groupId) drops.memberDragLeave(e, groupId);
  }

  function drop(e: DragEvent, groupId: string | null): void {
    if (drops && groupId) drops.memberDrop(e, groupId);
  }
</script>

{#each visibleNodes as node (sidebarVisibleRowKey(node))}
  <!-- role="presentation" keeps the animated wrapper transparent to the
       list > listitem structure now that the divider shares it. -->
  <div role="presentation" animate:sidebarFlip in:sidebarEnter out:sidebarExit>
    {#if node.kind !== 'group-more' && node.startsSection}
      <div
        role="separator"
        aria-hidden="true"
        data-testid="thread-section-divider"
        class="mx-2 my-1 border-t border-border-subtle/60"
      ></div>
    {/if}
    {#if node.kind === 'group-more'}
      <!-- Inside the group's rail and a drop target for the group,
           like the member rows above it. -->
      <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
      <div
        role="listitem"
        class="border-l border-border-subtle/60 pb-px -mb-px"
        style:margin-left="{GROUP_RAIL_LEFT_PX}px"
        ondragover={(e) => dragOver(e, node.groupId)}
        ondragleave={(e) => dragLeave(e, node.groupId)}
        ondrop={(e) => drop(e, node.groupId)}
      >
        <ThreadListMoreFooter
          hiddenCount={node.hiddenCount}
          hiddenStatus={node.hiddenStatus}
          canShowLess={node.canShowLess}
          testIdPrefix="thread-group"
          onShowMore={() => showMoreGroup(node.groupId)}
          onShowLess={() => collapseThreadList(node.groupId)}
        />
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
        ondragover={(e) => dragOver(e, node.ownerGroupId)}
        ondragleave={(e) => dragLeave(e, node.ownerGroupId)}
        ondrop={(e) => drop(e, node.ownerGroupId)}
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

{#if hiddenThreadCount > 0 || preview.canShowLess}
  <ThreadListMoreFooter
    hiddenCount={hiddenThreadCount}
    {hiddenStatus}
    canShowLess={preview.canShowLess}
    testIdPrefix="project-thread-list"
    onShowMore={showMoreTopLevel}
    onShowLess={collapseTopLevel}
  />
{/if}
