<script lang="ts">
  import {
    getJumpHintsVisible,
    jumpLabelForThread,
  } from '../../stores/keyboardModifiers.svelte';
  import { formatChord, keybindingForCommand } from '../../stores/keybindings.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { openThreadInPane } from '../../stores/panes.svelte';
  import {
    getEffectiveThreadStatus,
    type ThreadLiveStatus,
  } from '../../stores/threadStatuses.svelte';
  import type { Thread } from '../../types/models';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { relativeTime } from '../../utils/format';
  import ThreadRowActions from './ThreadRowActions.svelte';
  import ThreadRowBadges from './ThreadRowBadges.svelte';
  import ThreadRowForkAffordance from './ThreadRowForkAffordance.svelte';
  import ThreadContextMenu from './ThreadContextMenu.svelte';
  import ThreadRowPinButton from './ThreadRowPinButton.svelte';
  import {
    archiveThreadAction,
    renameThreadAction,
    unarchiveThreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';
  import {
    hasUnread,
    resolveThreadStatusPill,
    type ThreadStatusPill,
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
    displayLiveStatus,
    displayStatus,
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
    /**
     * Optional discussion-tree rollup status. When present, the row renders
     * the most important child status while retaining the contributing
     * child's pill label/color.
     */
    displayLiveStatus?: ThreadLiveStatus;
    displayStatus?: ThreadStatusPill | null;
  } = $props();

  let isActive = $derived(pane.threadId === thread.id);

  let liveStatus = $derived(getEffectiveThreadStatus(thread));
  let effectiveStatus = $derived(displayLiveStatus ?? liveStatus);
  let pill = $derived(displayStatus !== undefined ? displayStatus : resolveThreadStatusPill(thread, effectiveStatus));
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
      switchPane: async (t) => { await openThreadInPane(t, pane); },
      reportError: (msg) => pane.setGeneralError(msg),
      replacePaneThread: (t) => pane.replaceThread(t),
    };
  }

  async function handleJumpToParent(e: MouseEvent): Promise<void> {
    e.stopPropagation();
    if (!forkParent) return;
    await openThreadInPane(forkParent, pane);
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
    void openThreadInPane(thread, pane);
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

  // The pin affordance only shows for top-level rows. Nested discussion
  // children don't pin individually — the parent thread is the pin
  // target for that whole subtree.
  let showPinAffordance = $derived(indent <= 1);
  let isPinned = $derived(thread.pinnedAt != null);

  // Jump-hint label for this row when the user holds Cmd/Ctrl. Reactive:
  // when modifier press fires, the keyboardModifiers store rescans the
  // DOM and updates the labels map — Svelte re-derives this row's label
  // and the pill renders.
  let jumpLabel = $derived.by<string | null>(() => {
    if (!getJumpHintsVisible()) return null;
    return jumpLabelForThread(thread.id) ?? null;
  });
  let jumpShortcut = $derived.by<string | null>(() => {
    if (!jumpLabel) return null;
    return formatChord(keybindingForCommand(`thread.jump.${jumpLabel}`) ?? `mod+${jumpLabel}`);
  });

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

  // Indent scale for discussion children. Depth 1 (top-level threads
  // under a project) sits flush against the rail container's padding —
  // the rail itself provides the visual nesting cue. Depths 2-3 step
  // 8px per level, with a clamp at depth 3 so malformed deep chains
  // can't push titles off-screen.
  const INDENT_PX = [0, 0, 8, 16];
  let indentPx = $derived(INDENT_PX[Math.min(indent, INDENT_PX.length - 1)]);
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
    {selected ? 'bg-accent/15 text-fg' : isActive ? 'bg-accent/10 text-fg' : 'text-fg-muted hover:bg-surface-2/30 hover:text-fg'}
    {pill?.glowClass ?? ''}"
  style="padding-left: {indentPx}px"
  data-testid="thread-row"
  data-sidebar-thread-id={thread.id}
  data-live-status={liveStatus}
  data-effective-status={effectiveStatus}
>
  {#if showPinAffordance}
    <ThreadRowPinButton {isPinned} buildCtx={ctx} />
  {/if}
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

  {#if thread.forkedFromThreadId}
    <ThreadRowForkAffordance {forkParent} onJumpToParent={handleJumpToParent} />
  {/if}

  {#if pill}
    <span
      class="w-1.5 h-1.5 rounded-full shrink-0 {pill.dotClass} {pill.pulse ? 'animate-pulse' : ''}"
      role="status"
      aria-label={pill.label}
      title={pill.label}
      data-testid="thread-row-status-dot"
      data-status={effectiveStatus}
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
      aria-label="Rename Thread"
      class="text-xs flex-1 min-w-0 bg-surface-0 border border-accent/50 rounded-[var(--radius-field)] px-1 py-0.5 text-fg focus:outline-none"
      onclick={(e) => e.stopPropagation()}
    />
  {:else}
    <span
      data-testid="thread-row-title"
      class="text-xs truncate flex-1 min-w-0 {unread ? 'font-semibold text-fg' : ''}"
    >
      {thread.title || 'Untitled'}
    </span>
    <ThreadRowBadges {thread} />

    <!--
      Right-side slot. A fixed min-w-12 keeps the layout stable when the
      time label fades out on hover and the archive button fades in.
      Both live in `relative` so the button can absolute-position over
      the time without pushing layout.
    -->
    <div class="ml-auto relative shrink-0 flex items-center justify-end min-w-12">
      {#if jumpShortcut}
        <!--
          Modifier-held jump-hint pill. Fades in on the right side,
          replacing the relative-time stamp. The shown keybinding navigates to
          this row when active.
        -->
        <span
          class="inline-flex h-5 items-center rounded-[var(--radius-field)] border border-border-subtle bg-surface-1/90 px-1.5 font-mono text-[10px] font-medium text-fg shadow-sm pointer-events-none"
          aria-hidden="true"
          data-testid="thread-row-jump-hint"
        >
          {jumpShortcut}
        </span>
      {:else}
        <span
          class="text-[10px] tabular-nums text-fg-hint transition-opacity duration-150 pointer-events-none group-hover/thread-row:opacity-0 group-focus-within/thread-row:opacity-0"
          data-testid="thread-row-time"
        >
          {relativeTime(thread.updatedAt, getSettings().timestampFormat)}
        </span>
      {/if}
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
  title="Archive Thread"
  description="This will hide the thread from the sidebar. Toggle 'Include archived' and use the Unarchive action to bring it back."
  confirmLabel="Archive"
  onConfirm={() => { showArchiveConfirm = false; void archiveThreadAction(ctx()); }}
  onCancel={() => { showArchiveConfirm = false; }}
/>
