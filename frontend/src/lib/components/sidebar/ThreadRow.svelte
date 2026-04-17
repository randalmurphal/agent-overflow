<script lang="ts">
  import { ArchiveThread, DeleteThread, ForkThread, GitRemoveWorktree, RenameThread, StopSession, UnarchiveThread } from '../../stores/bindings';
  import { getSettings } from '../../stores/settings.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadById, prependThread, removeThread, replaceThread, updateThreadTitle } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { Thread } from '../../types/models';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { relativeTime } from '../../utils/format';

  let {
    thread,
    pane,
    onStartDiscussion,
    selected = false,
    onSelectClick,
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
  } = $props();

  let isActive = $derived(pane.threadId === thread.id);
  let canStartDiscussion = $derived(
    thread.interactionMode !== 'discussion' && !thread.discussionId && !thread.parentThreadId,
  );

  // Fork ancestry for the lineage badge. We look the parent up on every
  // render; the threads store is a tiny array so the O(n) find is cheap,
  // and a derived is preferable to stashing the parent on the row prop.
  let forkParent = $derived.by<Thread | undefined>(() => {
    if (!thread.forkedFromThreadId) return undefined;
    return getThreadById(thread.forkedFromThreadId);
  });

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
    const newTitle = editValue.trim();
    if (!newTitle || newTitle === thread.title) {
      editing = false;
      return;
    }

    saving = true;
    try {
      await RenameThread(thread.id, newTitle);
      updateThreadTitle(thread.id, newTitle);
    } catch (err) {
      console.error('Failed to rename thread:', err);
      pane.setError(`Failed to rename thread: ${err}`);
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

  async function doArchive() {
    try {
      // Stop the session before archiving so the provider process is cleaned up.
      // Best-effort: log if it fails but proceed with archive.
      await StopSession(thread.id).catch((err) => {
        console.error('Failed to stop session before archive:', err);
      });
      await ArchiveThread(thread.id);
      removeThread(thread.id);
      if (isActive) {
        pane.clear();
      }
    } catch (err) {
      console.error('Failed to archive thread:', err);
      pane.setError(`Failed to archive thread: ${err}`);
    }
  }

  function handleArchive(e: MouseEvent) {
    e.stopPropagation();
    if (getSettings().confirmArchive) {
      showArchiveConfirm = true;
    } else {
      doArchive();
    }
  }

  async function handleUnarchive(e: MouseEvent) {
    e.stopPropagation();
    try {
      const restored = (await UnarchiveThread(thread.id)) as Thread;
      // Patch the in-memory list so the row loses its archived style
      // immediately. The sidebar's filter uses the `archived` flag directly.
      replaceThread(restored);
      addToast('info', `Unarchived "${restored.title || 'thread'}"`);
    } catch (err) {
      console.error('Failed to unarchive thread:', err);
      pane.setError(`Failed to unarchive thread: ${err}`);
    }
  }

  async function doDelete() {
    try {
      // Stop the session before deleting so the provider process is cleaned up.
      // Best-effort: log if it fails but proceed with delete.
      await StopSession(thread.id).catch((err) => {
        console.error('Failed to stop session before delete:', err);
      });
      if (thread.worktreePath) {
        await GitRemoveWorktree(thread.id);
      }
      await DeleteThread(thread.id);
      removeThread(thread.id);
      if (isActive) {
        pane.clear();
      }
    } catch (err) {
      console.error('Failed to delete thread:', err);
      pane.setError(`Failed to delete thread: ${err}`);
    }
  }

  function handleDelete(e: MouseEvent) {
    e.stopPropagation();
    if (getSettings().confirmDelete) {
      showDeleteConfirm = true;
    } else {
      doDelete();
    }
  }

  async function handleFork(e: MouseEvent) {
    e.stopPropagation();
    try {
      const forked = await ForkThread(thread.id) as Thread;
      prependThread(forked);
      await pane.switchThread(forked);
      addToast('info', `Forked "${thread.title}" into a new thread`);
    } catch (err) {
      console.error('Failed to fork thread:', err);
      pane.setError(`Failed to fork thread: ${err}`);
    }
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
  class="group w-full text-left px-3 py-2 rounded-md cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50
    {selected ? 'bg-accent/25 text-text-primary ring-1 ring-accent/50' : isActive ? 'bg-accent/15 text-text-primary' : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary'}"
>
  <div class="flex items-center gap-2">
    <span class="text-[10px] font-bold px-1 py-0.5 rounded shrink-0
      {thread.provider === 'claude' ? 'bg-accent/20 text-accent' : 'bg-provider-codex/20 text-provider-codex'}" aria-hidden="true">
      {thread.provider === 'claude' ? 'C' : 'X'}
    </span>

    {#if editing}
      <!-- svelte-ignore a11y_autofocus -->
      <input
        bind:this={inputEl}
        bind:value={editValue}
        onkeydown={handleRenameKeydown}
        onblur={saveRename}
        disabled={saving}
        aria-label="Rename thread"
        class="text-sm flex-1 min-w-0 bg-surface-0 border border-accent rounded px-1 py-0.5 text-text-primary focus:outline-none"
        onclick={(e) => e.stopPropagation()}
      />
    {:else}
      <span class="text-sm truncate flex-1">{thread.title || 'Untitled'}</span>
      {#if thread.interactionMode === 'discussion'}
        <span class="text-[9px] px-1 py-0.5 rounded bg-accent/15 text-accent/80 shrink-0" title="Discussion parent thread" aria-label="Discussion parent thread">D</span>
      {:else if thread.parentThreadId}
        <span class="text-[9px] px-1 py-0.5 rounded bg-provider-codex/15 text-provider-codex/80 shrink-0" title="Discussion participant" aria-label="Discussion participant">Dp</span>
      {:else if thread.interactionMode === 'design'}
        <span class="text-[9px] px-1 py-0.5 rounded bg-provider-codex/15 text-provider-codex/90 shrink-0" title="Design mode thread" aria-label="Design mode thread">Dsn</span>
      {/if}
      {#if thread.worktreePath}
        <span class="text-[9px] px-1 py-0.5 rounded bg-accent/15 text-accent/70 shrink-0" title="Worktree: {thread.worktreePath}">WT</span>
      {/if}
      {#if thread.forkedFromThreadId}
        <button
          type="button"
          data-testid="thread-row-fork-lineage"
          onclick={handleJumpToParent}
          disabled={!forkParent}
          class="text-[9px] px-1 py-0.5 rounded bg-provider-codex/15 text-provider-codex/80 shrink-0 cursor-pointer disabled:cursor-not-allowed disabled:opacity-60 hover:bg-provider-codex/25 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50"
          title={forkParent
            ? `Forked from "${forkParent.title || 'Untitled'}" — click to open parent`
            : 'Forked thread (parent not loaded in sidebar)'}
          aria-label="Fork lineage"
        >
          F↩
        </button>
      {/if}
    {/if}

    {#if !editing}
      {#if onStartDiscussion && canStartDiscussion}
        <button
          onclick={handleStartDiscussion}
          class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
          aria-label="Start discussion on thread"
          title="Start discussion"
        >
          <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
          </svg>
        </button>
      {/if}
      <button
        onclick={handleFork}
        disabled={!thread.sessionRef}
        class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer disabled:cursor-not-allowed disabled:opacity-0 focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
        aria-label="Fork thread"
        title={thread.sessionRef ? 'Fork thread' : 'Fork available after the thread has provider state'}
      >
        <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <circle cx="6" cy="6" r="2" />
          <circle cx="18" cy="6" r="2" />
          <circle cx="18" cy="18" r="2" />
          <path d="M8 6h7" />
          <path d="M18 8v8" />
          <path d="M8 7.5c4 1 7 4 8 8" />
        </svg>
      </button>
      <button
        onclick={handleDelete}
        class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-error/60 hover:text-error text-xs px-1 shrink-0 cursor-pointer focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
        aria-label="Delete thread"
      >
        <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
        </svg>
      </button>
      {#if thread.archived}
        <button
          onclick={handleUnarchive}
          data-testid="thread-row-unarchive"
          class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
          aria-label="Unarchive thread"
          title="Unarchive thread"
        >
          <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M21 8v13H3V8M1 3h22v5H1z" />
            <path d="M9 12h6" />
            <path d="M12 9v6" />
          </svg>
        </button>
      {:else}
        <button
          onclick={handleArchive}
          data-testid="thread-row-archive"
          class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
          aria-label="Archive thread"
        >
          <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M21 8v13H3V8M1 3h22v5H1zM10 12h4" />
          </svg>
        </button>
      {/if}
    {/if}
  </div>
  <div class="text-xs text-text-secondary/60 mt-0.5 ml-6">
    {relativeTime(thread.updatedAt, getSettings().timestampFormat)}
  </div>
</div>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete thread"
  description="This will permanently delete this thread and all its messages. This action cannot be undone."
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => { showDeleteConfirm = false; doDelete(); }}
  onCancel={() => { showDeleteConfirm = false; }}
/>

<ConfirmDialog
  open={showArchiveConfirm}
  title="Archive thread"
  description="This will hide the thread from the sidebar. Toggle 'Include archived' and use the Unarchive action to bring it back."
  confirmLabel="Archive"
  onConfirm={() => { showArchiveConfirm = false; doArchive(); }}
  onCancel={() => { showArchiveConfirm = false; }}
/>
