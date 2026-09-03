<script lang="ts">
  import {
    getJumpHintsVisible,
    jumpLabelForThread,
  } from '../../stores/keyboardModifiers.svelte';
  import { chordHintForCommand } from '../../stores/keybindings.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { clearSidebarCursor, getSidebarCursorThreadId } from '../../stores/sidebarCursor.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadById, getThreadLiveActivityAt } from '../../stores/threads.svelte';
  import { getMinuteNow } from '../../stores/minuteClock.svelte';
  import {
    findPaneShowingThread,
    openThreadFromNavigation,
    openThreadInNewPane,
    openThreadInPane,
  } from '../../stores/panes.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import {
    getEffectiveThreadStatus,
    type ThreadLiveStatus,
  } from '../../stores/threadStatuses.svelte';
  import type { Thread } from '../../types/models';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import FileText from '@lucide/svelte/icons/file-text';
  import FolderGit2 from '@lucide/svelte/icons/folder-git-2';
  import Terminal from '@lucide/svelte/icons/terminal';
  import Icon from '../primitives/Icon.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ThreadRowActions from './ThreadRowActions.svelte';
  import ThreadRowBadges from './ThreadRowBadges.svelte';
  import ThreadRowForkAffordance from './ThreadRowForkAffordance.svelte';
  import ThreadContextMenu from './ThreadContextMenu.svelte';
  import ThreadRowPinButton from './ThreadRowPinButton.svelte';
  import {
    archiveThreadAction,
    deleteThreadAction,
    PIN_GROUP_BACK,
    PIN_GROUP_FRONT,
    pinThreadAction,
    renameThreadAction,
    setThreadPinGroupAction,
    unpinThreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';
  import {
    hasUnread,
    resolveThreadStatusPill,
    type ThreadStatusPill,
  } from '../../utils/threadStatusPill';
  import { pathBasename } from '../../utils/pathDisplay';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import {
    beginThreadRowDrag,
    encodeThreadDragPayload,
    endThreadRowDrag,
    THREAD_ROW_DRAG_MIME,
    type ThreadDragPayload,
  } from '../../utils/threadDragPayload';
  import { sidebarRowPaddingLeftPx, sidebarTimeLabel } from '../../utils/sidebarRowMetrics';

  let {
    thread,
    pane,
    selected = false,
    onSelectClick,
    indent = 0,
    inGroup = false,
    hasChildren = false,
    expanded = false,
    onToggleExpand,
    displayLiveStatus,
    displayStatus,
  }: {
    thread: Thread;
    pane: ThreadPane | null;
    selected?: boolean;
    /**
     * Called before the thread-switch path when the user clicks the row.
     * Return `true` to suppress the thread switch (row handled as a
     * multi-select action instead). `modifier` describes what the click
     * should do: 'range' (shift) or null / 'single'. Cmd/Ctrl-click is
     * reserved for opening/focusing a pane.
     */
    onSelectClick?: (modifier: 'toggle' | 'range' | 'single' | null) => boolean;
    /** Visual indent level. 0 = top, 1 = direct child of a discussion parent. */
    indent?: number;
    /** Rendered inside a group's rail: no pin gutter (see sidebarRowMetrics). */
    inGroup?: boolean;
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

  // isActive: this thread is what the FOCUSED pane shows (the sidebar's
  // `pane` prop). isOpen: it is mounted in some pane, focused or not.
  let isActive = $derived(pane?.threadId === thread.id);
  let isOpen = $derived(findPaneShowingThread(thread.id) !== null);
  let isCursorTarget = $derived(getSidebarCursorThreadId() === thread.id);
  // Terminals aren't archivable — the row offers Delete (X) instead of
  // Archive, and the leading glyph is the terminal icon.
  let isTerminal = $derived(thread.mode === 'terminal');

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
      clearPane: () => pane?.clear(),
      switchPane: async (t) => {
        if (pane) await openThreadInPane(t, pane);
        else await openThreadInPane(t);
      },
      reportError: (msg) => {
        if (pane) pane.setGeneralError(msg);
        else addToast('error', msg);
      },
      replacePaneThread: (t) => pane?.replaceThread(t),
    };
  }

  async function handleJumpToParent(e: MouseEvent): Promise<void> {
    e.stopPropagation();
    if (!forkParent) return;
    if (pane) await openThreadFromNavigation(forkParent, pane);
    else await openThreadFromNavigation(forkParent);
  }

  // Inline rename state (owned by the row so the <input> replaces the
  // title span in place — same pattern as ProjectItem).
  let editing = $state(false);
  let editValue = $state('');
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let saving = $state(false);

  // Archive-confirm dialog (shown when settings.confirmArchive is true).
  // The right-click Delete path's confirm dialog lives inside
  // ThreadContextMenu; the terminal row-X's lives here (showDeleteConfirm).
  let showArchiveConfirm = $state(false);
  // Delete-confirm dialog for the terminal row-X (shown when
  // settings.confirmDelete is true).
  let showDeleteConfirm = $state(false);

  // Context menu anchor + state.
  let rowEl: HTMLDivElement | undefined = $state(undefined);
  let ctxOpen = $state(false);

  function handleChevronClick(e: MouseEvent): void {
    e.stopPropagation();
    onToggleExpand?.();
  }

  function handleClick(e?: MouseEvent) {
    if (editing) return;
    // A mouse click is an unambiguous "I'm picking THIS thread" — drop
    // the visual cursor so it doesn't drift from where the user is.
    clearSidebarCursor();
    if (e && (e.metaKey || e.ctrlKey)) {
      void openThreadInNewPane(thread);
      return;
    }
    if (onSelectClick && e) {
      const modifier: 'toggle' | 'range' | 'single' | null = e.shiftKey
        ? 'range'
        : null;
      if (modifier !== null) {
        const handled = onSelectClick(modifier);
        if (handled) return;
      }
    }
    if (pane) void openThreadFromNavigation(thread, pane);
    else void openThreadFromNavigation(thread);
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
    // Enter confirms the IME candidate while composing a CJK title; committing
    // the rename here would save the pre-composition text and exit edit mode.
    if (e.key === 'Enter' && isImeComposingEvent(e)) return;
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

  function handleDelete(e: MouseEvent) {
    e.stopPropagation();
    if (getSettings().confirmDelete) {
      showDeleteConfirm = true;
    } else {
      void deleteThreadAction(ctx());
    }
  }

  // The pin affordance only shows for top-level rows that are not in a
  // group. Nested discussion children don't pin individually — the parent
  // thread is the pin target for that whole subtree — and a grouped thread
  // cannot hold a pin at all (one pin per visible row: the GROUP carries it,
  // and the schema refuses a pin on a grouped row).
  let showPinAffordance = $derived(indent <= 1 && !thread.groupId);
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
    // null when that slot's command is unbound — the pill is a hint for a
    // chord that exists, so it renders nothing rather than a chord the
    // user cleared.
    return chordHintForCommand(`thread.jump.${jumpLabel}`);
  });

  // The row's activity stamp. getThreadLiveActivityAt folds in the
  // per-thread streaming box (row objects no longer churn per beat),
  // and getMinuteNow keeps an idle row's relative label creeping
  // forward now that unrelated beats no longer re-render every row.
  let timeLabel = $derived.by(() => {
    getMinuteNow();
    return sidebarTimeLabel(getThreadLiveActivityAt(thread));
  });

  function handleContextMenu(e: MouseEvent) {
    // Cancel an in-flight rename so the menu anchor is a stable row
    // element and the input doesn't swallow the menu's outside-click.
    if (editing) cancelRename();
    e.preventDefault();
    e.stopPropagation();
    ctxOpen = true;
  }

  function handleDragStart(e: DragEvent): void {
    if (editing || !e.dataTransfer) {
      e.preventDefault();
      return;
    }
    const payload: ThreadDragPayload = {
      threadId: thread.id,
      title: thread.title || 'Untitled',
      projectId: thread.projectId ?? '',
      ...(thread.groupId ? { groupId: thread.groupId } : {}),
    };
    e.dataTransfer.effectAllowed = 'copyMove';
    e.dataTransfer.setData(THREAD_ROW_DRAG_MIME, encodeThreadDragPayload(payload));
    e.dataTransfer.setData('text/plain', thread.id);
    // The group drop targets read this during dragover, where the
    // DataTransfer is in protected mode and hands back nothing.
    beginThreadRowDrag(payload);

    const ghost = document.createElement('div');
    ghost.className = 'fixed -top-96 left-0 flex items-center gap-2 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1 text-xs text-fg shadow-menu';
    if (pill) {
      const dot = document.createElement('span');
      dot.className = `h-2 w-2 rounded-full ${pill.dotClass}`;
      ghost.appendChild(dot);
    }
    const title = document.createElement('span');
    title.textContent = thread.title || 'Untitled';
    ghost.appendChild(title);
    document.body.appendChild(ghost);
    e.dataTransfer.setDragImage(ghost, 8, 8);
    window.setTimeout(() => ghost.remove(), 0);
  }

  function closeCtxMenu() {
    ctxOpen = false;
  }

  // Indent + pin gutter come from utils/sidebarRowMetrics so a group row and
  // the member rows under it line up to the pixel.
  let rowPaddingLeftPx = $derived(sidebarRowPaddingLeftPx(indent, inGroup));

  let worktreeName = $derived(pathBasename(thread.worktreePath));
  let showWorktreeMeta = $derived(!editing && Boolean(thread.worktreePath && worktreeName));
  const WORKTREE_META_OFFSET_PX = 14;
  let worktreeIndentPx = $derived(rowPaddingLeftPx + WORKTREE_META_OFFSET_PX);
  let worktreeRightPaddingPx = $derived(52);
</script>

<!--
  Open-thread marker: a 2px accent bar on the shell's left edge (left of
  the pin gutter) for every thread mounted in some pane; the focused
  pane's thread adds a fill on top. Both derive from the theme's accent
  token. The keyboard cursor keeps its own inset ring, so the two never
  share a channel. The bar is the shell's ::after because the approval /
  input glow (`.status-glow-*` in app.css) owns ::before: an open row
  that is also blocked on the user needs both, and one pseudo-element
  cannot be a 2px bar and a full-row ring at once.
-->
<div
  class="group/thread-item relative rounded-[var(--radius-field)] transition-colors
    {selected ? 'bg-accent/15' : isActive ? 'bg-accent/20' : 'hover:bg-surface-2/30'}
    {isOpen ? 'after:absolute after:left-0 after:inset-y-1 after:w-0.5 after:rounded-full after:bg-accent' : ''}
    {isOpen && !isActive ? 'after:opacity-70' : ''}
    {isCursorTarget ? 'ring-1 ring-accent/70 ring-inset' : (pill?.ringClass ?? '')}
    {pill?.glowClass ?? ''}"
  data-testid="thread-row-shell"
  data-open={isOpen || null}
  data-focused={isActive || null}
  data-cursor-target={isCursorTarget || null}
>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    bind:this={rowEl}
    onclick={(e) => handleClick(e)}
    ondblclick={startRename}
    oncontextmenu={handleContextMenu}
    ondragstart={handleDragStart}
    ondragend={endThreadRowDrag}
    onkeydown={(e) => { if (!editing && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); handleClick(); } if (!editing && e.key === 'F2') { e.preventDefault(); startRename(); } }}
    role="button"
    tabindex={0}
    draggable={!editing}
    aria-pressed={selected}
    class="group/thread-row relative flex items-center gap-1.5 h-6 pr-1 rounded-[var(--radius-field)] cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40
      {selected || isOpen ? 'text-fg' : 'text-fg-muted group-hover/thread-item:text-fg'}"
    style="padding-left: {rowPaddingLeftPx}px"
    data-testid="thread-row"
    data-sidebar-thread-id={thread.id}
    data-live-status={liveStatus}
    data-effective-status={effectiveStatus}
  >
  {#if showPinAffordance}
    <!--
      Leading pin slot. Absolutely positioned inside the row's reserved
      pin gutter (PIN_SLOT_PX of padding-left) so the pin sits in the
      gap between the project rail and the row content without
      contributing a flex gap of its own. The wrapper is non-interactive;
      the button inside opts back in to pointer events when visible.
    -->
    <div class="absolute inset-y-0 left-0 flex items-center justify-center w-6 pointer-events-none">
      <ThreadRowPinButton
        {isPinned}
        pinGroup={thread.pinGroup}
        pinLabel="Pin Thread"
        unpinLabel="Unpin Thread"
        onToggle={() => { if (isPinned) void unpinThreadAction(ctx()); else void pinThreadAction(ctx()); }}
        onCycleBurner={() => void setThreadPinGroupAction(
          ctx(),
          thread.pinGroup === PIN_GROUP_BACK ? PIN_GROUP_FRONT : PIN_GROUP_BACK,
        )}
      />
    </div>
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
    {#if isTerminal}
      <!--
        Terminal rows render the boxless lucide `Terminal` glyph (a `>_`
        prompt) — the SAME glyph the chat history shows for bash/terminal
        tool calls (ToolKindIcon kind="terminal"), in the matching green
        `text-ico-terminal` — so a terminal reads as "a terminal" at a glance.

        This is checked BEFORE the draft branch on purpose. The backend marks
        any thread with no persisted items as a draft (store.go: IsDraft is
        "no items exist for the thread"), and a terminal is a degenerate,
        item-less thread, so it reaches this row with isDraft === true. Mode is
        the stronger signal — a terminal is never a composer draft — so it must
        win the icon slot regardless of the draft flag.
      -->
      <span
        class="inline-flex items-center shrink-0 text-ico-terminal"
        data-testid="thread-row-terminal-icon"
        aria-label="Terminal thread"
        title="Terminal"
      >
        <Icon icon={Terminal} size={11} strokeWidth={1.75} />
      </span>
    {:else if thread.isDraft}
      <span
        class="inline-flex items-center shrink-0 text-fg-subtle"
        data-testid="thread-row-draft-icon"
        aria-label="Draft thread"
        title="Draft"
      >
        <Icon icon={FileText} size={11} strokeWidth={1.75} class="opacity-70" />
      </span>
    {/if}
    <span
      data-testid="thread-row-title"
      class="text-xs truncate flex-1 min-w-0 {unread ? 'font-semibold text-fg' : ''}"
      title={thread.title || 'Untitled'}
    >
      {thread.title || 'Untitled'}
    </span>
    <ThreadRowBadges {thread} />

    <!--
      Right-side slot. A fixed min-w-7 keeps the layout stable when the
      time label fades out and the action button (archive or delete)
      fades in on hover / keyboard focus. Both live in `relative` so the
      button can absolute-position over the time without pushing layout.
      The keyboard reveal keys off `group-has-[:focus-visible]/thread-row`
      (a focus-VISIBLE descendant) rather than `:focus-within`, so a mouse
      click on the tabindex=0 row doesn't leave the action stuck visible.
    -->
    <div class="ml-auto relative shrink-0 flex items-center justify-end min-w-7">
      {#if jumpShortcut}
        <!--
          Modifier-held jump-hint pill. Fades in on the right side,
          replacing the relative-time stamp. The shown keybinding navigates to
          this row when active.
        -->
        <span
          class="inline-flex h-5 items-center rounded-[var(--radius-field)] border border-border-subtle bg-surface-1/90 px-1.5 font-mono text-[0.625rem] font-medium text-fg shadow-sheet pointer-events-none"
          aria-hidden="true"
          data-testid="thread-row-jump-hint"
        >
          {jumpShortcut}
        </span>
      {:else}
        <span
          class="text-[0.625rem] tabular-nums text-fg-hint transition-opacity duration-150 pointer-events-none group-hover/thread-item:opacity-0 group-has-[:focus-visible]/thread-row:opacity-0"
          data-testid="thread-row-time"
        >
          {timeLabel}
        </span>
        <!--
          Hover actions stay unmounted while the jump-hint pill is up —
          otherwise the absolutely-positioned archive/delete button paints
          over the ctrl+# pill on the hovered row.
        -->
        <div
          class="absolute inset-y-0 right-0 flex items-center opacity-0 pointer-events-none transition-opacity duration-150 group-hover/thread-item:opacity-100 group-hover/thread-item:pointer-events-auto group-has-[:focus-visible]/thread-row:opacity-100 group-has-[:focus-visible]/thread-row:pointer-events-auto"
        >
          <ThreadRowActions
            onArchive={isTerminal ? undefined : handleArchive}
            onDelete={isTerminal ? handleDelete : undefined}
          />
        </div>
      {/if}
    </div>
  {/if}
  </div>

  {#if showWorktreeMeta}
    <div
      class="relative -mt-1.5 flex h-3.5 items-center text-[0.625rem] leading-none text-fg-hint"
      style="padding-left: {worktreeIndentPx}px; padding-right: {worktreeRightPaddingPx}px"
      title="Worktree: {thread.worktreePath}"
      aria-label="Worktree {worktreeName}"
      data-testid="thread-row-worktree"
    >
      <span
        class="inline-flex min-w-0 max-w-full items-center gap-1 px-1 py-0 text-fg-hint"
        data-testid="thread-row-worktree-label"
      >
        <Icon icon={FolderGit2} size={10} strokeWidth={1.8} class="shrink-0 opacity-85" />
        <span
          class="min-w-0 truncate font-mono text-[0.625rem]"
          data-testid="thread-row-worktree-name"
        >
          {worktreeName}
        </span>
      </span>
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
  description="This will hide the thread from the sidebar. Open Settings → Storage to find it later."
  confirmLabel="Archive"
  onConfirm={() => { showArchiveConfirm = false; void archiveThreadAction(ctx()); }}
  onCancel={() => { showArchiveConfirm = false; }}
/>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete Terminal"
  description="This will remove the terminal from the sidebar and close its shell. This cannot be undone."
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => { showDeleteConfirm = false; void deleteThreadAction(ctx()); }}
  onCancel={() => { showDeleteConfirm = false; }}
/>
