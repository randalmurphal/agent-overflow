<script lang="ts">
  import ArchiveRestore from 'lucide-svelte/icons/archive-restore';
  import Trash2 from 'lucide-svelte/icons/trash-2';
  import Icon from '../primitives/Icon.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import {
    DeleteThread,
    GitRemoveWorktree,
    ListArchivedThreads,
    StopSession,
    UnarchiveThread,
  } from '../../stores/bindings';
  import { prependThread } from '../../stores/threads.svelte';
  import { closePanesShowingThread } from '../../stores/panes.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';
  import { displayModelLabel } from '../../utils/modelLabels';
  import type { Thread } from '../../types/models';

  let archivedThreads: Thread[] = $state([]);
  let loading = $state(true);
  let loadError: string | null = $state(null);
  let deleteTarget: Thread | null = $state(null);

  $effect(() => {
    loading = true;
    loadError = null;
    ListArchivedThreads()
      .then((result) => {
        archivedThreads = (result ?? []) as Thread[];
      })
      .catch((err) => {
        console.error('Failed to load archived threads:', err);
        loadError = errString(err);
      })
      .finally(() => {
        loading = false;
      });
  });

  async function handleUnarchive(thread: Thread): Promise<void> {
    try {
      const restored = (await UnarchiveThread(thread.id)) as Thread;
      archivedThreads = archivedThreads.filter((t) => t.id !== thread.id);
      prependThread(restored);
      addToast('info', `Unarchived "${restored.title || 'thread'}".`);
    } catch (err) {
      console.error('Failed to unarchive thread:', err);
      addToast('error', `Failed to unarchive: ${errString(err)}`);
    }
  }

  async function handleDelete(thread: Thread): Promise<void> {
    try {
      await StopSession(thread.id).catch((err) => {
        console.error('Failed to stop session before delete:', err);
      });
      if (thread.worktreePath) {
        await GitRemoveWorktree(thread.id);
      }
      await DeleteThread(thread.id);
      archivedThreads = archivedThreads.filter((t) => t.id !== thread.id);
      closePanesShowingThread(thread.id);
      addToast('info', `Deleted "${thread.title || 'thread'}".`);
    } catch (err) {
      console.error('Failed to delete archived thread:', err);
      addToast('error', `Failed to delete: ${errString(err)}`);
    }
  }

  function confirmDelete(thread: Thread): void {
    deleteTarget = thread;
  }

  function cancelDelete(): void {
    deleteTarget = null;
  }

  function executeDelete(): void {
    if (!deleteTarget) return;
    const target = deleteTarget;
    deleteTarget = null;
    void handleDelete(target);
  }

  const rowBtnClass =
    'flex items-center justify-center h-6 w-6 rounded-[var(--radius-field)] shrink-0 ' +
    'cursor-pointer text-fg-subtle hover:text-fg hover:bg-surface-2/40 ' +
    'focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40 transition-colors';
</script>

<div class="flex flex-col gap-6">
  <section>
    <SettingsHeader
      eyebrow="Archive"
      title="Archived Threads"
      description="Archived threads are hidden from the sidebar. Unarchive to bring one back, or delete to remove it permanently."
    />
  </section>

  <section class="flex flex-col gap-1">
    {#if loading}
      <p class="text-[0.8125rem] text-fg-subtle">Loading...</p>
    {:else if loadError}
      <p class="text-[0.8125rem] text-red-400">Failed to load archived threads: {loadError}</p>
    {:else if archivedThreads.length === 0}
      <p class="text-[0.8125rem] text-fg-subtle">No archived threads.</p>
    {:else}
      <p class="text-[0.65625rem] font-medium uppercase tracking-[0.16em] text-fg-hint">
        {archivedThreads.length} archived {archivedThreads.length === 1 ? 'thread' : 'threads'}
      </p>
      <div class="mt-2 flex flex-col gap-px rounded-[var(--radius-field)] border border-border-subtle overflow-hidden">
        {#each archivedThreads as thread (thread.id)}
          <div class="group flex items-center gap-3 bg-surface-1/50 px-3 py-2.5 hover:bg-surface-2/30 transition-colors">
            <div class="min-w-0 flex-1">
              <p class="truncate text-[0.8125rem] font-medium text-fg">{thread.title || 'Untitled'}</p>
              <p class="mt-0.5 truncate text-[0.6875rem] text-fg-subtle">
				{thread.provider}{thread.model ? ` · ${displayModelLabel(thread.provider, thread.model)}` : ''}
                {#if thread.updatedAt}
                  <span class="text-fg-hint"> · {relativeTime(thread.updatedAt)}</span>
                {/if}
              </p>
            </div>
            <div class="flex items-center gap-1 shrink-0 opacity-0 group-hover:opacity-100 transition-opacity">
              <button
                type="button"
                onclick={() => handleUnarchive(thread)}
                class={rowBtnClass}
                aria-label="Unarchive"
                title="Unarchive"
              >
                <Icon icon={ArchiveRestore} size={13} strokeWidth={2} class="opacity-90" />
              </button>
              <button
                type="button"
                onclick={() => confirmDelete(thread)}
                class="{rowBtnClass} hover:!text-red-400"
                aria-label="Delete permanently"
                title="Delete permanently"
              >
                <Icon icon={Trash2} size={13} strokeWidth={2} class="opacity-90" />
              </button>
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </section>
</div>

<ConfirmDialog
  open={deleteTarget !== null}
  title="Delete Archived Thread"
  description="This will permanently delete this thread and all its messages. This action cannot be undone."
  confirmLabel="Delete"
  destructive={true}
  onConfirm={executeDelete}
  onCancel={cancelDelete}
/>
