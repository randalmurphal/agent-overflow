<script lang="ts">
  import { onMount } from 'svelte';
  import { ListThreads, ArchiveThread, DeleteThread } from '../../stores/bindings';
  import { getSettings } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { Thread } from '../../types/models';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { relativeTime } from '../../utils/format';

  let archived = $state<Thread[]>([]);
  let loading = $state(true);
  let deleteTarget = $state<Thread | null>(null);

  onMount(async () => {
    await loadArchived();
  });

  async function loadArchived() {
    loading = true;
    try {
      const all = await ListThreads() as Thread[];
      archived = all.filter((t) => t.archived);
    } catch (err) {
      console.error('Failed to load archived threads:', err);
      addToast('error', 'Failed to load archived threads');
    } finally {
      loading = false;
    }
  }

  async function handleUnarchive(thread: Thread) {
    try {
      await ArchiveThread(thread.id);
      archived = archived.filter((t) => t.id !== thread.id);
    } catch (err) {
      console.error('Failed to unarchive thread:', err);
      addToast('error', 'Failed to unarchive thread');
    }
  }

  async function handleDelete() {
    if (!deleteTarget) return;
    try {
      await DeleteThread(deleteTarget.id);
      archived = archived.filter((t) => t.id !== deleteTarget!.id);
    } catch (err) {
      console.error('Failed to delete thread:', err);
      addToast('error', 'Failed to delete thread');
    } finally {
      deleteTarget = null;
    }
  }
</script>

{#if loading}
  <p class="text-sm text-text-secondary animate-pulse">Loading archived threads...</p>
{:else if archived.length === 0}
  <div class="text-center py-8">
    <p class="text-sm text-text-secondary">No archived threads</p>
    <p class="text-xs text-text-secondary/60 mt-1">Archived threads will appear here</p>
  </div>
{:else}
  <div class="space-y-1">
    {#each archived as thread (thread.id)}
      <div class="flex items-center gap-3 px-3 py-2 rounded border border-border bg-surface-0">
        <span class="text-[10px] font-bold px-1 py-0.5 rounded shrink-0
          {thread.provider === 'claude' ? 'bg-accent/20 text-accent' : 'bg-provider-codex/20 text-provider-codex'}">
          {thread.provider === 'claude' ? 'C' : 'X'}
        </span>
        <div class="flex-1 min-w-0">
          <p class="text-sm text-text-primary truncate">{thread.title || 'Untitled'}</p>
          <p class="text-xs text-text-secondary/60">{relativeTime(thread.updatedAt, getSettings().timestampFormat)}</p>
        </div>
        <button
          onclick={() => handleUnarchive(thread)}
          class="text-xs px-2 py-1 rounded border border-border text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Unarchive
        </button>
        <button
          onclick={() => { deleteTarget = thread; }}
          class="text-xs px-2 py-1 rounded border border-error/40 text-error hover:bg-error/10 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Delete
        </button>
      </div>
    {/each}
  </div>
{/if}

<ConfirmDialog
  open={deleteTarget !== null}
  title="Delete archived thread"
  description="This will permanently delete this thread. This action cannot be undone."
  confirmLabel="Delete"
  destructive={true}
  onConfirm={handleDelete}
  onCancel={() => { deleteTarget = null; }}
/>
