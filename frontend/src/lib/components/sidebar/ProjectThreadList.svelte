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

  import type { Thread, ThreadGroup } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    shouldOpenProjectThreadInNewPane,
    type ProjectNewThreadHandler,
  } from './projectNewThread';
  import {
    collapseThreadList,
    getCollapsedGroups,
    getExpandedDiscussions,
    getThreadListVisibleLimit,
    isGroupExpanded,
    isThreadListExpanded,
    setCollapsedGroups,
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
  import { getThreadLiveActivityAt } from '../../stores/threads.svelte';
  import { openThreadIds } from '../../stores/panes.svelte';
  import { sidebarFlip, sidebarEnter, sidebarExit } from '../../utils/sidebarAnimate';
  import Plus from '@lucide/svelte/icons/plus';
  import Icon from '../primitives/Icon.svelte';
  import ThreadGroupRow from './ThreadGroupRow.svelte';
  import ThreadRow from './ThreadRow.svelte';
  import {
    buildSidebarThreadTree,
    sidebarTreeNodeId,
  } from '../../utils/sidebarTree';
  import {
    flattenSidebarThreadTree,
    nextSidebarThreadRevealLimit,
    previewSidebarThreads,
    rollupDisplayStatus,
    sameSidebarVisibleNodes,
    sameThreadStatusPill,
    syncExpandedTreeForActiveThread,
  } from '../../utils/sidebarTreeView';
  import { THREAD_PREVIEW_LIMIT, THREAD_REVEAL_INCREMENT } from '../../utils/sidebarThreadLimits';
  import { hasScope } from '../../transport/scopes';
  import {
    canDropThreadInGroup,
    canUngroupDroppedThread,
    endThreadRowDrag,
    threadDragPayloadForEvent,
  } from '../../utils/threadDragPayload';
  import {
    moveThreadsToGroupAction,
    removeThreadsFromGroupAction,
  } from './threadGroupActions';

  interface Props {
    projectId: string;
    threads: Thread[];
    /** The project's groups, already search-filtered by ProjectsSection. */
    groups?: readonly ThreadGroup[];
    pane: ThreadPane | null;
    /** Click handler for the empty-state "+ New Thread" button. */
    onNewThread?: ProjectNewThreadHandler;
  }

  let { projectId, threads, groups = [], pane, onNewThread }: Props = $props();
  let lastNewThreadContextMenuAt = 0;
  // Creating a thread rides `threads:operate`. The empty-state control is
  // the whole content of an empty project, so it stays and goes inert.
  let newThreadUngranted = $derived(!hasScope('threads:operate'));

  // Tree is built per-render: cheap (small N) and lets us reactively
  // pick up effective live-status changes from the status store and
  // live-activity ordering from the per-thread activity boxes. Streaming
  // beats DO wake this derived (activity is sort input); the identity
  // cutoff on visibleNodes below is what keeps those beats from
  // reaching the DOM.
  let tree = $derived(
    buildSidebarThreadTree({
      threads,
      groups,
      statusOf: (thread) => getEffectiveThreadStatus(thread),
      activityOf: (thread) => getThreadLiveActivityAt(thread),
    }),
  );

  // Threads mounted in any pane never hide behind the cut. Re-minted only
  // when a pane opens, closes, or swaps thread.
  let openIds = $derived(openThreadIds());
  let listExpanded = $derived(isThreadListExpanded(projectId));
  let visibleLimit = $derived(getThreadListVisibleLimit(projectId));

  // Truncation operates on top-level nodes only — discussion children
  // are nested under their parent and don't compete for preview slots.
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
  // return the previous one when its content is unchanged — otherwise
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

  // Auto-expand the chain of ancestors leading to the active thread so
  // a freshly-switched discussion participant shows up without a manual
  // chevron click. Drops expanded ids that no longer point at expandable
  // nodes (a child thread was deleted, parent is now a leaf).
  $effect(() => {
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: getExpandedDiscussions(),
      collapsedGroupIds: getCollapsedGroups(),
      activeThreadId: pane?.threadId ?? null,
    });
    setExpandedDiscussions(next.expandedThreadIds);
    // Both setters no-op on an equal set, so this effect settles after one
    // pass even though it reads the two stores it writes.
    setCollapsedGroups(next.collapsedGroupIds);
  });

  // Identity cutoff: return the PREVIOUS array when nothing the rows
  // render has changed, so svelte's derived cutoff stops the animated
  // each-block from reconciling. This is load-bearing for streaming
  // performance — without it every item-event flush re-ran the FLIP
  // measure pass (a forced synchronous layout per beat). See
  // sameSidebarVisibleNodes.
  let prevVisibleNodes: ReturnType<typeof flattenSidebarThreadTree> = [];
  let visibleNodes = $derived.by(() => {
    const next = flattenSidebarThreadTree({
      nodes: preview.visibleNodes,
      expandedThreadIds: getExpandedDiscussions(),
      collapsedGroupIds: getCollapsedGroups(),
    });
    if (sameSidebarVisibleNodes(prevVisibleNodes, next)) return prevVisibleNodes;
    prevVisibleNodes = next;
    return next;
  });

  function handleShowMore(e: MouseEvent): void {
    e.stopPropagation();
    const nextLimit = nextSidebarThreadRevealLimit({
      nodes: tree,
      openThreadIds: openIds,
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

  // ── Drop targets ─────────────────────────────────────────────────────────
  //
  // Two targets share this list. A GROUP (its own row, or any member row
  // inside it) takes a thread from the same project and moves it in; the LIST
  // BACKGROUND takes a grouped thread from this project and ungroups it.
  // The group targets stopPropagation, so a drop one of them handled never
  // also reaches the container — and while a group is lit, the container's
  // dashed outline stays down even though its own dragover stopped firing.
  // dropTargetGroupId is the group UNDER THE POINTER, whether or not it would
  // take the payload: a member dragged over its own group is refused, but the
  // group still owns the pointer, so the ungroup outline behind it must stay
  // down — that drop ungroups nothing. `dropTargetAccepts` is what lights the
  // group row.
  let dropTargetGroupId = $state<string | null>(null);
  let dropTargetAccepts = $state(false);
  let containerDragActive = $state(false);
  let showUngroupOutline = $derived(containerDragActive && dropTargetGroupId === null);

  function setGroupDropTarget(groupId: string, active: boolean, accepts = false): void {
    if (active) {
      dropTargetGroupId = groupId;
      dropTargetAccepts = accepts;
    } else if (dropTargetGroupId === groupId) {
      dropTargetGroupId = null;
      dropTargetAccepts = false;
    }
  }

  function clearDropState(): void {
    dropTargetGroupId = null;
    dropTargetAccepts = false;
    containerDragActive = false;
  }

  // Member-row targets. The row itself knows nothing about groups: the
  // wrapper around it is the target, and it lights the group, not the row.
  function memberDragOver(e: DragEvent, groupId: string): void {
    if (!e.dataTransfer) return;
    const payload = threadDragPayloadForEvent(e);
    // A thread dragged over a row of its OWN group is not an ungroup either —
    // swallow it rather than letting the container claim it.
    if (!payload) return;
    e.stopPropagation();
    const accepts = canDropThreadInGroup(payload, projectId, groupId);
    setGroupDropTarget(groupId, true, accepts);
    if (!accepts) {
      e.dataTransfer.dropEffect = 'none';
      return;
    }
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
  }

  function memberDragLeave(e: DragEvent, groupId: string): void {
    const next = e.relatedTarget as Node | null;
    if (next && e.currentTarget instanceof Node && e.currentTarget.contains(next)) return;
    setGroupDropTarget(groupId, false);
  }

  function memberDrop(e: DragEvent, groupId: string): void {
    const payload = threadDragPayloadForEvent(e);
    setGroupDropTarget(groupId, false);
    if (!payload) return;
    e.stopPropagation();
    endThreadRowDrag();
    if (!canDropThreadInGroup(payload, projectId, groupId)) return;
    e.preventDefault();
    void moveThreadsToGroupAction([payload.threadId], groupId);
  }

  // The list background: the ungroup target.
  function handleContainerDragOver(e: DragEvent): void {
    if (!e.dataTransfer) return;
    const payload = threadDragPayloadForEvent(e);
    if (!payload || !canUngroupDroppedThread(payload, projectId)) {
      e.dataTransfer.dropEffect = 'none';
      containerDragActive = false;
      return;
    }
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    containerDragActive = true;
  }

  function handleContainerDragLeave(e: DragEvent): void {
    // relatedTarget rather than an enter/leave counter: the container is
    // crossed constantly on the way between rows, and only a leave that
    // actually exits the element should drop the outline.
    const next = e.relatedTarget as Node | null;
    if (next && e.currentTarget instanceof Node && e.currentTarget.contains(next)) return;
    clearDropState();
  }

  function handleContainerDrop(e: DragEvent): void {
    const payload = threadDragPayloadForEvent(e);
    clearDropState();
    if (!payload) return;
    // No dragend is guaranteed: the source row unmounts if its project
    // collapsed or a search was typed while the drag was in the air.
    endThreadRowDrag();
    if (!canUngroupDroppedThread(payload, projectId)) return;
    e.preventDefault();
    void removeThreadsFromGroupAction([payload.threadId]);
  }

  function handleEmptyNewThreadContextMenu(e: MouseEvent): void {
    if (!shouldOpenProjectThreadInNewPane(e)) return;
    e.preventDefault();
    e.stopPropagation();
    lastNewThreadContextMenuAt = Date.now();
    onNewThread?.(projectId, { openInNewPane: true });
  }

</script>

{#if threads.length === 0 && groups.length === 0}
  <button
    type="button"
    onclick={handleEmptyNewThreadClick}
    oncontextmenu={handleEmptyNewThreadContextMenu}
    disabled={newThreadUngranted}
    title={newThreadUngranted ? 'Not granted to this device' : undefined}
    data-testid="project-thread-list-empty"
    class="ml-4 mr-2 my-1 inline-flex items-center gap-1 rounded-[var(--radius-field)] px-2 py-1 text-[0.6875rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-surface-2/0 disabled:hover:text-fg-hint"
  >
    <Icon icon={Plus} size={11} strokeWidth={2.2} class="opacity-80" />
    <span>New Thread</span>
  </button>
{:else}
  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div
    class="flex flex-col gap-px ml-2 border-l border-border-subtle/60 rounded-[var(--radius-field)]
      {showUngroupOutline ? 'outline-1 outline-dashed -outline-offset-1 outline-border-subtle' : ''}"
    role="list"
    aria-label="Project Threads"
    data-testid="project-thread-list"
    data-ungroup-target={showUngroupOutline || null}
    ondragover={handleContainerDragOver}
    ondragleave={handleContainerDragLeave}
    ondrop={handleContainerDrop}
    ondragend={clearDropState}
  >
    {#each visibleNodes as node (sidebarTreeNodeId(node))}
      <!-- role="presentation" keeps the animated wrapper transparent to the
           list > listitem structure now that the divider shares it. -->
      <div role="presentation" animate:sidebarFlip in:sidebarEnter out:sidebarExit>
        {#if node.startsBackBurnerBlock}
          <div
            role="separator"
            aria-hidden="true"
            data-testid="thread-pin-group-divider"
            class="mx-2 my-1 border-t border-border-subtle/60"
          ></div>
        {/if}
        {#if node.kind === 'group'}
          <div role="listitem">
            <ThreadGroupRow
              group={node.group}
              {pane}
              indent={node.depth + 1}
              expanded={isGroupExpanded(node.group.id)}
              memberThreadIds={node.children.filter((child) => child.kind === 'thread').map((child) => child.thread.id)}
              dropActive={dropTargetGroupId === node.group.id && dropTargetAccepts}
              onDropTargetChange={(active, accepts) => setGroupDropTarget(node.group.id, active, accepts)}
            />
          </div>
        {:else}
          <!--
            A group member sits behind its own rail, dropped from the centre
            of the group row's chevron (pin gutter 24px + half the 16px
            chevron = ml-8), so "inside the group" is a line and not an
            indent the eye has to measure. Rows are siblings in one animated list (no
            wrapper per group), so each member draws its own segment; the
            1px pad-and-pull-back carries the border across the list's
            gap-px so the segments join into one line.
          -->
          <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
          <div
            role="listitem"
            class={node.ownerGroupId ? 'ml-8 border-l border-border-subtle/60 pb-px -mb-px' : ''}
            data-group-member={node.ownerGroupId ? '' : null}
            ondragover={(e) => { if (node.ownerGroupId) memberDragOver(e, node.ownerGroupId); }}
            ondragleave={(e) => { if (node.ownerGroupId) memberDragLeave(e, node.ownerGroupId); }}
            ondrop={(e) => { if (node.ownerGroupId) memberDrop(e, node.ownerGroupId); }}
          >
            <ThreadRow
              thread={node.thread}
              {pane}
              indent={node.depth + 1}
              inGroup={node.ownerGroupId !== null}
              hasChildren={node.isExpandable}
              expanded={node.isExpanded}
              displayLiveStatus={node.displayLiveStatus}
              displayStatus={node.displayStatus}
              onToggleExpand={() => toggleDiscussion(node.thread.id)}
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
            class="group/more flex items-center gap-1.5 h-6 pl-6 pr-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
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
            class="flex items-center h-6 px-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          >
            Show Less
          </button>
        {/if}
      </div>
    {/if}
  </div>
{/if}
