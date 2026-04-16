<script lang="ts">
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { ArchiveThread, DeleteThread, RenameThread } from '../../stores/bindings';
  import { removeThread, updateThreadTitle } from '../../stores/threads.svelte';
  import { relativeTime } from '../../utils/format';
  import { getSettings } from '../../stores/settings.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';

  let { thread, pane }: { thread: Thread; pane: ThreadPane } = $props();

  let isActive = $derived(pane.threadId === thread.id);

  // Inline rename state
  let editing = $state(false);
  let editValue = $state('');
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let saving = $state(false);

  // Confirm dialogs
  let showDeleteConfirm = $state(false);
  let showArchiveConfirm = $state(false);

  function handleClick() {
    if (!editing) {
      pane.switchThread(thread);
    }
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

  async function doDelete() {
    try {
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
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  onclick={handleClick}
  ondblclick={startRename}
  onkeydown={(e) => { if (!editing && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); handleClick(); } }}
  role="button"
  tabindex={0}
  class="group w-full text-left px-3 py-2 rounded-md cursor-pointer transition-colors
    {isActive ? 'bg-accent/15 text-text-primary' : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary'}"
>
  <div class="flex items-center gap-2">
    <span class="text-[10px] font-bold px-1 py-0.5 rounded shrink-0
      {thread.provider === 'claude' ? 'bg-accent/20 text-accent' : 'bg-orange-900/30 text-orange-300'}">
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
        class="text-sm flex-1 min-w-0 bg-surface-0 border border-accent rounded px-1 py-0.5 text-text-primary focus:outline-none"
        onclick={(e) => e.stopPropagation()}
      />
    {:else}
      <span class="text-sm truncate flex-1">{thread.title || 'Untitled'}</span>
      {#if thread.worktreePath}
        <span class="text-[9px] px-1 py-0.5 rounded bg-accent/15 text-accent/70 shrink-0" title="Worktree: {thread.worktreePath}">WT</span>
      {/if}
    {/if}

    {#if !editing}
      <button
        onclick={handleDelete}
        class="opacity-0 group-hover:opacity-100 text-red-400/60 hover:text-red-400 text-xs px-1 shrink-0 cursor-pointer"
        title="Delete thread"
      >
        <svg class="w-3 h-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
          <path d="M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
        </svg>
      </button>
      <button
        onclick={handleArchive}
        class="opacity-0 group-hover:opacity-100 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer"
        title="Archive thread"
      >
        <svg class="w-3 h-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
          <path d="M21 8v13H3V8M1 3h22v5H1zM10 12h4" />
        </svg>
      </button>
    {/if}
  </div>
  <div class="text-xs text-text-secondary/60 mt-0.5 ml-6">
    {relativeTime(thread.updatedAt)}
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
  description="This thread will be moved to the archive. You can restore it later from Settings."
  confirmLabel="Archive"
  onConfirm={() => { showArchiveConfirm = false; doArchive(); }}
  onCancel={() => { showArchiveConfirm = false; }}
/>
