<script lang="ts">
  import { getSettings } from '../../stores/settings.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { getThreadStatus, type ThreadLiveStatus } from '../../stores/threadStatuses.svelte';
  import type { Thread } from '../../types/models';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { relativeTime } from '../../utils/format';
  import ThreadRowActions from './ThreadRowActions.svelte';
  import ThreadRowBadges from './ThreadRowBadges.svelte';
  import {
    archiveThreadAction,
    deleteThreadAction,
    forkThreadAction,
    renameThreadAction,
    unarchiveThreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';
  import { statusDotClass, statusDotLabel } from './threadRowStatus';

  let {
    thread,
    pane,
    onStartDiscussion,
    selected = false,
    onSelectClick,
    indent = 0,
    hasChildren = false,
    expanded = false,
    onToggleExpand,
  }: {
    thread: Thread;
    pane: ThreadPane;
    onStartDiscussion?: (thread: Thread) => void;
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

  function handleChevronClick(e: MouseEvent): void {
    e.stopPropagation();
    onToggleExpand?.();
  }

  let isActive = $derived(pane.threadId === thread.id);
  let canStartDiscussion = $derived(
    thread.mode !== 'discussion' && !thread.discussionId && !thread.parentThreadId,
  );

  // Fork ancestry for the lineage badge.
  let forkParent = $derived.by<Thread | undefined>(() => {
    if (!thread.forkedFromThreadId) return undefined;
    return getThreadById(thread.forkedFromThreadId);
  });

  let liveStatus: ThreadLiveStatus = $derived(getThreadStatus(thread.id));
  let dotClass = $derived(statusDotClass(liveStatus));
  let dotLabel = $derived(statusDotLabel(liveStatus));

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

  // Inline rename state
  let editing = $state(false);
  let editValue = $state('');
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let saving = $state(false);

  // Confirm dialogs
  let showDeleteConfirm = $state(false);
  let showArchiveConfirm = $state(false);

  function handleStartDiscussion(e: MouseEvent): void {
    e.stopPropagation();
    onStartDiscussion?.(thread);
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

  function handleDelete(e: MouseEvent) {
    e.stopPropagation();
    if (getSettings().confirmDelete) {
      showDeleteConfirm = true;
    } else {
      void deleteThreadAction(ctx());
    }
  }

  async function handleFork(e: MouseEvent) {
    e.stopPropagation();
    await forkThreadAction(ctx());
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  onclick={(e) => handleClick(e)}
  ondblclick={startRename}
  onkeydown={(e) => { if (!editing && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); handleClick(); } if (!editing && e.key === 'F2') { e.preventDefault(); startRename(); } }}
  role="button"
  tabindex={0}
  aria-pressed={selected}
  class="group w-full text-left px-3 py-1.5 rounded-[var(--radius-field)] cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40
    {selected ? 'bg-accent/15 text-fg border-l-2 border-accent pl-[10px]' : isActive ? 'bg-accent/8 text-fg border-l-2 border-accent pl-[10px]' : 'text-fg-muted hover:bg-surface-2/30 hover:text-fg'}"
  style="padding-left: calc(0.75rem + {indent * 0.9}rem)"
>
  <div class="flex items-center gap-2">
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
    {:else if indent > 0}
      <!-- Alignment spacer for children so their provider badge lines
           up with the parent's title column. -->
      <span class="w-4 h-4 shrink-0" aria-hidden="true"></span>
    {/if}
    <span class="text-[9px] font-semibold px-1 py-0.5 rounded-[4px] shrink-0 tracking-wide
      {thread.provider === 'claude' ? 'bg-accent/10 text-accent' : 'bg-provider-codex/10 text-provider-codex'}" aria-hidden="true">
      {thread.provider === 'claude' ? 'C' : 'X'}
    </span>
    {#if liveStatus === 'idle'}
      <!-- Transparent placeholder so the row doesn't shift when a dot
           appears/disappears as a thread moves through running/idle. -->
      <span class="w-2 h-2 shrink-0" aria-hidden="true" data-testid="thread-row-status-placeholder"></span>
    {:else}
      <span
        class="w-2 h-2 rounded-full shrink-0 {dotClass}"
        role="status"
        aria-label={dotLabel}
        title={dotLabel}
        data-testid="thread-row-status-dot"
        data-status={liveStatus}
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
        aria-label="Rename thread"
        class="text-[12.5px] flex-1 min-w-0 bg-surface-0 border border-accent/50 rounded-[var(--radius-field)] px-1 py-0.5 text-fg focus:outline-none"
        onclick={(e) => e.stopPropagation()}
      />
    {:else}
      <span class="text-[12.5px] truncate flex-1">{thread.title || 'Untitled'}</span>
      <ThreadRowBadges {thread} {forkParent} onJumpToParent={handleJumpToParent} />
    {/if}

    {#if !editing}
      <ThreadRowActions
        {thread}
        {canStartDiscussion}
        onStartDiscussion={onStartDiscussion ? handleStartDiscussion : undefined}
        onFork={handleFork}
        onDelete={handleDelete}
        onArchive={handleArchive}
        onUnarchive={handleUnarchive}
      />
    {/if}
  </div>
  <div class="text-[10px] text-fg-hint mt-0.5 ml-6">
    {relativeTime(thread.updatedAt, getSettings().timestampFormat)}
  </div>
</div>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete thread"
  description="This will permanently delete this thread and all its messages. This action cannot be undone."
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => { showDeleteConfirm = false; void deleteThreadAction(ctx()); }}
  onCancel={() => { showDeleteConfirm = false; }}
/>

<ConfirmDialog
  open={showArchiveConfirm}
  title="Archive thread"
  description="This will hide the thread from the sidebar. Toggle 'Include archived' and use the Unarchive action to bring it back."
  confirmLabel="Archive"
  onConfirm={() => { showArchiveConfirm = false; void archiveThreadAction(ctx()); }}
  onCancel={() => { showArchiveConfirm = false; }}
/>
