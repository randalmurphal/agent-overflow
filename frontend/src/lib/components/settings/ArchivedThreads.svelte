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

<section class="rounded-2xl border border-border/70 bg-surface-1/75 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
  <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Archive</p>
  <h3 class="mt-1 text-base font-semibold text-text-primary">Stored threads</h3>
  <p class="mt-1 text-sm text-text-secondary">Restore archived work back into the sidebar or delete it permanently.</p>
</section>

{#if loading}
  <div class="rounded-2xl border border-border/70 bg-surface-1/75 px-5 py-8 text-center shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
    <p class="text-sm text-text-secondary animate-pulse">Loading archived threads...</p>
  </div>
{:else if archived.length === 0}
  <div class="rounded-2xl border border-dashed border-border/70 bg-surface-1/60 px-5 py-10 text-center shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
    <div class="mx-auto flex h-12 w-12 items-center justify-center rounded-2xl border border-border/60 bg-surface-0/70 text-text-secondary/70">
      <svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M21 8v13H3V8M1 3h22v5H1zM10 12h4" />
      </svg>
    </div>
    <p class="mt-4 text-sm font-medium text-text-primary">No archived threads</p>
    <p class="mt-1 text-sm text-text-secondary">Archived threads will appear here once they leave the active sidebar.</p>
  </div>
{:else}
  <div class="space-y-3" role="list" aria-label="Archived threads">
    {#each archived as thread (thread.id)}
      <div role="listitem" class="flex items-center gap-3 rounded-2xl border border-border/70 bg-surface-1/75 px-4 py-3 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
        <span class="text-[10px] font-bold px-1 py-0.5 rounded shrink-0
          {thread.provider === 'claude' ? 'bg-accent/20 text-accent' : 'bg-provider-codex/20 text-provider-codex'}">
          {thread.provider === 'claude' ? 'C' : 'X'}
        </span>
        <div class="flex-1 min-w-0">
          <p class="text-sm text-text-primary truncate">{thread.title || 'Untitled'}</p>
          <p class="text-xs text-text-secondary/60 mt-1">{relativeTime(thread.updatedAt, getSettings().timestampFormat)}</p>
        </div>
        <div class="flex items-center gap-2">
          <button
            onclick={() => handleUnarchive(thread)}
            aria-label="Unarchive {thread.title || 'Untitled'}"
            class="text-xs px-3 py-1.5 rounded-xl border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            Unarchive
          </button>
          <button
            onclick={() => { deleteTarget = thread; }}
            aria-label="Delete {thread.title || 'Untitled'}"
            class="text-xs px-3 py-1.5 rounded-xl border border-error/40 text-error hover:bg-error/10 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            Delete
          </button>
        </div>
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
