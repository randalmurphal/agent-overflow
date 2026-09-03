<script lang="ts">
  // A thread group's own row. It is a SIBLING of ThreadRow, not a new widget:
  // same 24px height, same leading pin gutter and indent scale
  // (utils/sidebarRowMetrics), the same chevron button, the same status dot +
  // label markup, the same inline-rename flow. What differs is what the row
  // is FOR — a group has no thread of its own, so everything it shows (the
  // status pill, the activity stamp, the sort position) is bubbled from its
  // members by the tree builder and arrives here as props.
  //
  // The row body toggles expansion, the way a project header does; there is
  // nothing else to open. Members are rendered by ProjectThreadList as
  // ordinary ThreadRows one indent level in, not as children of this element.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ThreadGroup } from '../../types/models';
  import { getMinuteNow } from '../../stores/minuteClock.svelte';
  import { toggleGroup } from '../../stores/sidebar.svelte';
  import { getThreadById, getThreadLiveActivityAt } from '../../stores/threads.svelte';
  import { consumePendingGroupRename } from '../../stores/threadGroups.svelte';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Folder from '@lucide/svelte/icons/folder';
  import FolderOpen from '@lucide/svelte/icons/folder-open';
  import Icon from '../primitives/Icon.svelte';
  import ThreadGroupContextMenu from './ThreadGroupContextMenu.svelte';
  import ThreadRowPinButton from './ThreadRowPinButton.svelte';
  import {
    moveThreadsToGroupAction,
    pinThreadGroupAction,
    renameThreadGroupAction,
    setThreadGroupPinGroupAction,
    unpinThreadGroupAction,
  } from './threadGroupActions';
  import { PIN_GROUP_BACK, PIN_GROUP_FRONT } from './threadRowActions';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { sidebarRowPaddingLeftPx, sidebarTimeLabel } from '../../utils/sidebarRowMetrics';
  import {
    canDropThreadInGroup,
    endThreadRowDrag,
    threadDragPayloadForEvent,
  } from '../../utils/threadDragPayload';

  interface Props {
    group: ThreadGroup;
    pane: ThreadPane | null;
    /** Visual indent level, same scale ThreadRow uses. */
    indent?: number;
    expanded: boolean;
    /** Top-level member thread ids, in render order. The count is this length. */
    memberThreadIds: readonly string[];
    /**
     * True while a droppable thread hovers this group — including over one of
     * its member rows, which is why the state is owned by ProjectThreadList
     * and handed back down rather than kept here.
     */
    dropActive?: boolean;
    /**
     * Reports this row's own hover enter/leave to that owner. `accepts` is
     * false for a payload this group refuses (its own member): the pointer is
     * still OVER the group, so the container behind must not offer to ungroup
     * underneath it, but nothing here lights up.
     */
    onDropTargetChange?: (active: boolean, accepts: boolean) => void;
  }

  let {
    group,
    pane,
    indent = 0,
    expanded,
    memberThreadIds,
    dropActive = false,
    onDropTargetChange,
  }: Props = $props();

  let rowEl: HTMLDivElement | undefined = $state(undefined);
  let ctxOpen = $state(false);

  let memberCount = $derived(memberThreadIds.length);
  let isPinned = $derived(group.pinnedAt != null);
  let rowPaddingLeftPx = $derived(sidebarRowPaddingLeftPx(indent));

  // Same contract as ThreadRow's stamp, and read the same way: from the
  // members' own live-activity boxes. The tree's latestActivityAt is
  // deliberately not compared by sameSidebarVisibleNodes, so taking it as a
  // prop would freeze this label at whatever the last render-changing beat
  // left behind while a member streams. An empty group falls back to its own
  // last write, which is when it was created or renamed.
  let timeLabel = $derived.by(() => {
    getMinuteNow();
    let latest = 0;
    for (const id of memberThreadIds) {
      const member = getThreadById(id);
      if (!member) continue;
      const at = getThreadLiveActivityAt(member);
      if (at > latest) latest = at;
    }
    return sidebarTimeLabel(latest || (group.updatedAt ?? 0));
  });

  // ── Inline rename ────────────────────────────────────────────────────────
  let editing = $state(false);
  let editValue = $state('');
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let saving = $state(false);

  function startRename(): void {
    editing = true;
    editValue = group.name;
    requestAnimationFrame(() => {
      inputEl?.focus();
      inputEl?.select();
    });
  }

  function cancelRename(): void {
    editing = false;
  }

  async function saveRename(): Promise<void> {
    if (saving) return;
    // A blank name is not a rename the backend will take, and round-tripping
    // to be told so would cost a toast the user did not earn. Treat it the
    // way Escape is treated.
    if (!editValue.trim()) {
      cancelRename();
      return;
    }
    saving = true;
    try {
      await renameThreadGroupAction(group, editValue);
    } finally {
      saving = false;
      editing = false;
    }
  }

  function handleRenameKeydown(e: KeyboardEvent): void {
    // Enter confirms the IME candidate while composing; committing here
    // would save the pre-composition text and leave edit mode.
    if (e.key === 'Enter' && isImeComposingEvent(e)) return;
    if (e.key === 'Enter') {
      e.preventDefault();
      void saveRename();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelRename();
    }
  }

  // A group is born named "New Group" with its rename already requested, so
  // the first thing its row does is open the editor over that placeholder.
  $effect(() => {
    if (consumePendingGroupRename(group.id)) startRename();
  });

  // ── Expansion + menu ─────────────────────────────────────────────────────
  function toggleExpansion(): void {
    if (editing) return;
    toggleGroup(group.id);
  }

  function handleChevronClick(e: MouseEvent): void {
    e.stopPropagation();
    toggleExpansion();
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (editing) return;
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      toggleExpansion();
      return;
    }
    if (e.key === 'F2') {
      e.preventDefault();
      startRename();
    }
  }

  function handleContextMenu(e: MouseEvent): void {
    if (editing) cancelRename();
    e.preventDefault();
    e.stopPropagation();
    ctxOpen = true;
  }

  // ── Drop target ──────────────────────────────────────────────────────────
  // dragenter/dragleave fire per descendant element, so a plain leave handler
  // would drop the highlight the moment the pointer crossed the chevron. The
  // counter is the standard fix: the row is left only when every enter has
  // been matched.
  let dragDepth = 0;

  function acceptsDrag(e: DragEvent): boolean {
    const payload = threadDragPayloadForEvent(e);
    return payload !== null && canDropThreadInGroup(payload, group.projectId, group.id);
  }

  /**
   * A thread drag over this row is THIS row's business even when it is
   * refused — a member dragged over its own group must not fall through to
   * the container behind, which would offer to ungroup it.
   */
  function claimsDrag(e: DragEvent): boolean {
    return threadDragPayloadForEvent(e) !== null;
  }

  function clearDropActive(): void {
    dragDepth = 0;
    onDropTargetChange?.(false, false);
  }

  function handleDragEnter(e: DragEvent): void {
    if (!claimsDrag(e)) return;
    e.stopPropagation();
    dragDepth += 1;
    const accepts = acceptsDrag(e);
    if (accepts) e.preventDefault();
    onDropTargetChange?.(true, accepts);
  }

  function handleDragOver(e: DragEvent): void {
    if (!e.dataTransfer) return;
    // The container behind this row ungroups; nothing this row has an opinion
    // about may also reach it.
    if (!claimsDrag(e)) return;
    e.stopPropagation();
    const accepts = acceptsDrag(e);
    onDropTargetChange?.(true, accepts);
    if (!accepts) {
      // No preventDefault: the drop is refused outright, and the cursor says so.
      e.dataTransfer.dropEffect = 'none';
      return;
    }
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
  }

  function handleDragLeave(): void {
    dragDepth -= 1;
    if (dragDepth <= 0) clearDropActive();
  }

  function handleDrop(e: DragEvent): void {
    const payload = threadDragPayloadForEvent(e);
    clearDropActive();
    if (!payload) return;
    e.stopPropagation();
    // The drag is over whether or not this group takes it, and the source row
    // may have unmounted mid-drag, so no dragend is coming to clear the record.
    endThreadRowDrag();
    if (!canDropThreadInGroup(payload, group.projectId, group.id)) return;
    e.preventDefault();
    void moveThreadsToGroupAction([payload.threadId], group.id);
  }
</script>

<div
  class="group/thread-item relative rounded-[var(--radius-field)] transition-colors hover:bg-surface-2/30
    {dropActive ? 'bg-accent/15 ring-1 ring-accent/40 ring-inset' : ''}"
  data-testid="thread-group-row-shell"
  data-drop-active={dropActive || null}
  ondragenter={handleDragEnter}
  ondragover={handleDragOver}
  ondragleave={handleDragLeave}
  ondrop={handleDrop}
  role="presentation"
>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    bind:this={rowEl}
    onclick={toggleExpansion}
    ondblclick={startRename}
    oncontextmenu={handleContextMenu}
    onkeydown={handleKeydown}
    role="button"
    tabindex={0}
    draggable={false}
    aria-expanded={expanded}
    class="group/thread-row relative flex items-center gap-1.5 h-6 pr-1 rounded-[var(--radius-field)] cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    style="padding-left: {rowPaddingLeftPx}px"
    data-testid="thread-group-row"
    data-sidebar-group-id={group.id}
    data-expanded={expanded}
  >
    <!-- Leading pin slot, identical to ThreadRow's: absolutely placed in the
         reserved gutter so it costs the flex row no gap of its own. -->
    <div class="absolute inset-y-0 left-0 flex items-center justify-center w-6 pointer-events-none">
      <ThreadRowPinButton
        {isPinned}
        pinGroup={group.pinGroup}
        pinLabel="Pin Group"
        unpinLabel="Unpin Group"
        onToggle={() => {
          if (isPinned) void unpinThreadGroupAction(group.id);
          else void pinThreadGroupAction(group.id);
        }}
        onCycleBurner={() => void setThreadGroupPinGroupAction(
          group.id,
          group.pinGroup === PIN_GROUP_BACK ? PIN_GROUP_FRONT : PIN_GROUP_BACK,
        )}
      />
    </div>

    <!-- Always rendered, even for an empty group: the chevron is what says
         "this row contains things", and a group that shows one only once it
         has a member reads as a different kind of row each time. -->
    <button
      type="button"
      onclick={handleChevronClick}
      data-testid="thread-group-row-expand"
      aria-label={expanded ? 'Collapse group' : 'Expand group'}
      aria-expanded={expanded}
      class="flex items-center justify-center w-4 h-4 rounded text-fg-subtle hover:text-fg hover:bg-surface-2/30 shrink-0 cursor-pointer focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40"
    >
      <Icon
        icon={ChevronRight}
        size={11}
        strokeWidth={2.5}
        class={'opacity-80 transition-transform ' + (expanded ? 'rotate-90' : '')}
      />
    </button>

    <span class="inline-flex items-center shrink-0 text-fg-subtle" aria-hidden="true">
      <Icon icon={expanded ? FolderOpen : Folder} size={11} strokeWidth={1.75} />
    </span>

    {#if editing}
      <input
        bind:this={inputEl}
        bind:value={editValue}
        onkeydown={handleRenameKeydown}
        onblur={saveRename}
        disabled={saving}
        aria-label="Rename Group"
        class="text-xs flex-1 min-w-0 bg-surface-0 border border-accent/50 rounded-[var(--radius-field)] px-1 py-0.5 text-fg focus:outline-none"
        onclick={(e) => e.stopPropagation()}
      />
    {:else}
      <span
        data-testid="thread-group-row-name"
        class="text-xs font-medium truncate flex-1 min-w-0 text-fg-muted group-hover/thread-item:text-fg"
        title={group.name}
      >
        {group.name}
      </span>
      <div class="ml-auto relative shrink-0 flex items-center justify-end min-w-7">
        {#if !expanded}
          <span
            class="text-[0.625rem] tabular-nums text-fg-hint"
            data-testid="thread-group-row-count"
          >
            {memberCount}
          </span>
        {:else if memberCount > 0}
          <span
            class="text-[0.625rem] tabular-nums text-fg-hint"
            data-testid="thread-group-row-time"
          >
            {timeLabel}
          </span>
        {/if}
      </div>
    {/if}
  </div>
</div>

<ThreadGroupContextMenu
  {group}
  {pane}
  anchor={rowEl}
  open={ctxOpen}
  onClose={() => { ctxOpen = false; }}
  onRename={startRename}
/>
