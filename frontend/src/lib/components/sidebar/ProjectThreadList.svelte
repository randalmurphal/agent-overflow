<script lang="ts">
  // Thread list for a single project: its pending forks, its empty state,
  // and the list element whose rows SidebarTreeList renders, with the drop
  // targets only a project's list has (a group takes a thread in, the list
  // background ungroups one).
  import { pendingForks } from '../../stores/forkPreparation.svelte';
  import ForkPendingRow from './ForkPendingRow.svelte';
  import { projectHasScope } from '../../transport/entityScopes';
  import type { Thread, ThreadGroup } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    shouldOpenProjectThreadInNewPane,
    type ProjectNewThreadHandler,
  } from './projectNewThread';
  import Plus from '@lucide/svelte/icons/plus';
  import Icon from '../primitives/Icon.svelte';
  import SidebarTreeList, { type SidebarTreeDrops } from './SidebarTreeList.svelte';
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
    /** The project entry this list is. */
    projectId: string;
    threads: Thread[];
    /** The project's groups, already search-filtered by ProjectsSection. */
    groups?: readonly ThreadGroup[];
    /** The assistants with a row in this project (sidebarBuckets). */
    agents?: readonly Thread[];
    pane: ThreadPane | null;
    /** Click handler for the empty-state "+ New Thread" button. */
    onNewThread?: ProjectNewThreadHandler;
  }

  let { projectId, threads, groups = [], agents = [], pane, onNewThread }: Props = $props();
  let lastNewThreadContextMenuAt = 0;
  // Creating a thread rides `threads:operate`. The empty-state control is
  // the whole content of an empty project, so it stays and goes inert.
  let newThreadUngranted = $derived(!projectHasScope('threads:operate', projectId));

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

  // An agent row, its member rows and its done footer: a drop here moves
  // nothing (an agent adopts only what it spawned), with the same
  // affordance as a drop on another project. Swallowed so the container
  // never reads it as an ungroup.
  function agentDragOver(e: DragEvent): void {
    if (!e.dataTransfer || !threadDragPayloadForEvent(e)) return;
    e.stopPropagation();
    e.preventDefault();
    e.dataTransfer.dropEffect = 'none';
    clearDropState();
  }

  function agentDrop(e: DragEvent): void {
    if (!threadDragPayloadForEvent(e)) return;
    e.stopPropagation();
    e.preventDefault();
    endThreadRowDrag();
    clearDropState();
  }

  const drops: SidebarTreeDrops = {
    memberDragOver,
    memberDragLeave,
    memberDrop,
    agentDragOver,
    agentDrop,
    groupLit: (groupId) => dropTargetGroupId === groupId && dropTargetAccepts,
    groupTargetChange: setGroupDropTarget,
  };

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
</script>

{#each threads.filter(thread => thread.forkPreparing) as thread (thread.id)}
  <ForkPendingRow title={thread.title} />
{/each}

{#each pendingForks(projectId).filter(source => !threads.some(thread => thread.forkPreparing && thread.forkedFromThreadId === source.id)) as source (source.id)}
  <ForkPendingRow title={`${source.title} (fork)`} />
{/each}

{#if threads.length === 0 && groups.length === 0 && agents.length === 0}
  <div class="ml-4 flex flex-wrap items-center">
  <button
    type="button"
    onclick={handleEmptyNewThreadClick}
    oncontextmenu={handleEmptyNewThreadContextMenu}
    disabled={newThreadUngranted}
    title={newThreadUngranted ? 'Not granted to this device' : undefined}
    data-testid="project-thread-list-empty"
    class="mr-2 my-1 inline-flex items-center gap-1 rounded-[var(--radius-field)] px-2 py-1 compact:py-2 text-[0.6875rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-surface-2/0 disabled:hover:text-fg-hint"
  >
    <Icon icon={Plus} size={11} strokeWidth={2.2} class="opacity-80" />
    <span>New Thread</span>
  </button>
  </div>
{:else}
  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div
    class="flex flex-col gap-px ml-4 border-l border-border-subtle/60 rounded-[var(--radius-field)]
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
    <SidebarTreeList {threads} {groups} {agents} scope={projectId} listKey={projectId} {pane} {drops} />
  </div>
{/if}
