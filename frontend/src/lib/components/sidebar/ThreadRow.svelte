<script lang="ts">
  import { getSettings } from '../../stores/settings.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { getThreadStatus } from '../../stores/threadStatuses.svelte';
  import type { Thread } from '../../types/models';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { relativeTime } from '../../utils/format';
  import ThreadRowActions from './ThreadRowActions.svelte';
  import ThreadRowBadges from './ThreadRowBadges.svelte';
  import ThreadContextMenu from './ThreadContextMenu.svelte';
  import {
    archiveThreadAction,
    renameThreadAction,
    unarchiveThreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';
  import {
    hasUnread,
    resolveThreadStatusPill,
  } from './threadStatusPill';

  let {
    thread,
    pane,
    selected = false,
    onSelectClick,
    indent = 0,
    hasChildren = false,
    expanded = false,
    onToggleExpand,
  }: {
    thread: Thread;
    pane: ThreadPane;
    selected?: boolean;
    /**
     * Called before the thread-switch path when the user clicks the row.
     * Return `true` to suppress the thread switch (row handled as a
     * multi-select action instead). `modifier` describes what the click
     * should do: 'toggle' (cmd/ctrl), 'range' (shift), or null / 'single'.
     */
    onSelectClick?: (modifier: 'toggle' | 'range' | 'single' | null) => boolean;
    /** Visual indent level. 0 = top, 1 = direct child of a discussion parent. */
    indent?: number;
    /** True when this row represents a parent with at least one child below it. */
    hasChildren?: boolean;
    /** Controls the chevron direction when hasChildren is true. */
    expanded?: boolean;
    /** Fires on chevron click; caller toggles the expansion store. */
    onToggleExpand?: () => void;
  } = $props();

  let isActive = $derived(pane.threadId === thread.id);

  let liveStatus = $derived(getThreadStatus(thread.id));
  let pill = $derived(resolveThreadStatusPill(thread, liveStatus));
  let unread = $derived(hasUnread(thread));

  let forkParent = $derived.by<Thread | undefined>(() => {
    if (!thread.forkedFromThreadId) return undefined;
    return getThreadById(thread.forkedFromThreadId);
  });

  function ctx(): ThreadActionCtx {
    return {
      thread,
      isActive,
      clearPane: () => pane.clear(),
      switchPane: (t) => pane.switchThread(t),
      reportError: (msg) => pane.setGeneralError(msg),
    };
  }

  async function handleJumpToParent(e: MouseEvent): Promise<void> {
    e.stopPropagation();
    if (!forkParent) return;
    await pane.switchThread(forkParent);
  }

  // Inline rename state (owned by the row so the <input> replaces the
  // title span in place — same pattern as ProjectItem).
  let editing = $state(false);
  let editValue = $state('');
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let saving = $state(false);

  // Archive-confirm dialog (shown when settings.confirmArchive is true).
  // Delete's confirm dialog lives inside ThreadContextMenu.
  let showArchiveConfirm = $state(false);

  // Context menu anchor + state.
  let rowEl: HTMLDivElement | undefined = $state(undefined);
  let ctxOpen = $state(false);

  function handleChevronClick(e: MouseEvent): void {
    e.stopPropagation();
    onToggleExpand?.();
  }

  function handleClick(e?: MouseEvent) {
    if (editing) return;
    if (onSelectClick && e) {
      const modifier: 'toggle' | 'range' | 'single' | null = (e.metaKey || e.ctrlKey)
        ? 'toggle'
        : e.shiftKey
        ? 'range'
        : null;
      if (modifier !== null) {
        const handled = onSelectClick(modifier);
        if (handled) return;
      }
    }
    pane.switchThread(thread);
  }

  function startRename() {
    editing = true;
    editValue = thread.title || '';
    requestAnimationFrame(() => {
      inputEl?.focus();
      inputEl?.select();
    });
  }

  async function saveRename() {
    if (saving) return;
    saving = true;
    try {
      await renameThreadAction(ctx(), editValue);
    } finally {
      saving = false;
      editing = false;
    }
  }

  function cancelRename() {
    editing = false;
  }

  function handleRenameKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault();
      saveRename();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelRename();
    }
  }

  function handleArchive(e: MouseEvent) {
    e.stopPropagation();
    if (getSettings().confirmArchive) {
      showArchiveConfirm = true;
    } else {
      void archiveThreadAction(ctx());
    }
  }

  async function handleUnarchive(e: MouseEvent) {
    e.stopPropagation();
    await unarchiveThreadAction(ctx());
  }

  function handleContextMenu(e: MouseEvent) {
    // Cancel an in-flight rename so the menu anchor is a stable row
    // element and the input doesn't swallow the menu's outside-click.
    if (editing) cancelRename();
    e.preventDefault();
    e.stopPropagation();
    ctxOpen = true;
  }

  function closeCtxMenu() {
    ctxOpen = false;
  }

  // Indent scale for discussion children. Cap at 2 so a deeply nested
  // participant doesn't push the title off-screen in a narrow sidebar.
  const INDENT_PX = [0, 12, 24];
  let indentPx = $derived(INDENT_PX[Math.min(indent, 2)]);
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  bind:this={rowEl}
  onclick={(e) => handleClick(e)}
  ondblclick={startRename}
  oncontextmenu={handleContextMenu}
  onkeydown={(e) => { if (!editing && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); handleClick(); } if (!editing && e.key === 'F2') { e.preventDefault(); startRename(); } }}
  role="button"
  tabindex={0}
  aria-pressed={selected}
  class="group/thread-row relative flex items-center gap-1.5 h-7 pr-1 rounded-[var(--radius-field)] cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40
    {selected ? 'bg-accent/15 text-fg' : isActive ? 'bg-accent/10 text-fg' : 'text-fg-muted hover:bg-surface-2/30 hover:text-fg'}"
  style="padding-left: {8 + indentPx}px"
  data-testid="thread-row"
>
  {#if hasChildren}
    <button
      type="button"
      onclick={handleChevronClick}
      data-testid="thread-row-expand"
      aria-label={expanded ? 'Collapse participants' : 'Expand participants'}
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
  {/if}

  {#if pill}
    <span
      class="w-1.5 h-1.5 rounded-full shrink-0 {pill.dotClass} {pill.pulse ? 'animate-pulse' : ''}"
      role="status"
      aria-label={pill.label}
      title={pill.label}
      data-testid="thread-row-status-dot"
      data-status={liveStatus}
    ></span>
    <span
      class="text-[10px] font-medium whitespace-nowrap shrink-0 hidden min-[260px]:inline {pill.labelClass}"
      aria-hidden="true"
    >
      {pill.label}
    </span>
  {/if}

  {#if editing}
    <!-- svelte-ignore a11y_autofocus -->
    <input
      bind:this={inputEl}
      bind:value={editValue}
      onkeydown={handleRenameKeydown}
      onblur={saveRename}
      disabled={saving}
      aria-label="Rename thread"
      class="text-xs flex-1 min-w-0 bg-surface-0 border border-accent/50 rounded-[var(--radius-field)] px-1 py-0.5 text-fg focus:outline-none"
      onclick={(e) => e.stopPropagation()}
    />
  {:else}
    <span
      class="text-xs truncate flex-1 min-w-0 {unread ? 'font-semibold text-fg' : ''}"
    >
      {thread.title || 'Untitled'}
    </span>
    <ThreadRowBadges {thread} {forkParent} onJumpToParent={handleJumpToParent} />

    <!--
      Right-side slot. A fixed min-w-12 keeps the layout stable when the
      time label fades out on hover and the archive button fades in.
      Both live in `relative` so the button can absolute-position over
      the time without pushing layout.
    -->
    <div class="ml-auto relative shrink-0 flex items-center justify-end min-w-12">
      <span
        class="text-[10px] tabular-nums text-fg-hint transition-opacity duration-150 pointer-events-none group-hover/thread-row:opacity-0 group-focus-within/thread-row:opacity-0"
        data-testid="thread-row-time"
      >
        {relativeTime(thread.updatedAt, getSettings().timestampFormat)}
      </span>
      <div
        class="absolute inset-y-0 right-0 flex items-center opacity-0 pointer-events-none transition-opacity duration-150 group-hover/thread-row:opacity-100 group-hover/thread-row:pointer-events-auto group-focus-within/thread-row:opacity-100 group-focus-within/thread-row:pointer-events-auto"
      >
        <ThreadRowActions
          {thread}
          onArchive={handleArchive}
          onUnarchive={handleUnarchive}
        />
      </div>
    </div>
  {/if}
</div>

<ThreadContextMenu
  {thread}
  {pane}
  anchor={rowEl}
  open={ctxOpen}
  onClose={closeCtxMenu}
  onRename={startRename}
  {isActive}
/>

<ConfirmDialog
  open={showArchiveConfirm}
  title="Archive thread"
  description="This will hide the thread from the sidebar. Toggle 'Include archived' and use the Unarchive action to bring it back."
  confirmLabel="Archive"
  onConfirm={() => { showArchiveConfirm = false; void archiveThreadAction(ctx()); }}
  onCancel={() => { showArchiveConfirm = false; }}
/>
