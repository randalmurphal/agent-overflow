<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { GitStatus, GitActionResult } from '../../types/git';
  import { GetGitStatus, GitPush, GitPull } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import CommitDialog from './CommitDialog.svelte';
  import { onMount } from 'svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let status = $state<GitStatus | null>(null);
  let actionLoading = $state(false);
  let showCommit = $state(false);
  let showDropdown = $state(false);

  onMount(() => {
    refreshStatus();
  });

  async function refreshStatus() {
    if (!pane.threadId) return;
    try {
      const result = await GetGitStatus(pane.threadId);
      status = result as GitStatus;
    } catch (err) {
      console.error('Failed to get git status:', err);
      status = null;
    }
  }

  let primaryAction = $derived.by((): { label: string; action: string; disabled: boolean; tooltip: string } => {
    if (!status) return { label: 'Commit', action: 'commit', disabled: true, tooltip: 'Loading...' };
    if (status.hasChanges) return { label: 'Commit', action: 'commit', disabled: false, tooltip: 'Stage and commit changes' };
    if (status.aheadCount > 0) return { label: 'Push', action: 'push', disabled: false, tooltip: `Push ${status.aheadCount} commit${status.aheadCount !== 1 ? 's' : ''}` };
    if (status.behindCount > 0) return { label: 'Pull', action: 'pull', disabled: false, tooltip: `Pull ${status.behindCount} commit${status.behindCount !== 1 ? 's' : ''}` };
    return { label: 'Commit', action: 'commit', disabled: true, tooltip: 'No changes to commit' };
  });

  async function executePrimary() {
    switch (primaryAction.action) {
      case 'commit':
        showCommit = true;
        break;
      case 'push':
        await doPush();
        break;
      case 'pull':
        await doPull();
        break;
    }
  }

  async function doPush() {
    if (!pane.threadId || actionLoading) return;
    actionLoading = true;
    try {
      const result = await GitPush(pane.threadId);
      const r = result as GitActionResult;
      if (r.error) {
        pane.setError(`Push failed: ${r.error}`);
      } else {
        addToast('success', 'Pushed successfully');
        await refreshStatus();
      }
    } catch (err) {
      pane.setError(`Push failed: ${err}`);
    } finally {
      actionLoading = false;
    }
  }

  async function doPull() {
    if (!pane.threadId || actionLoading) return;
    actionLoading = true;
    try {
      const result = await GitPull(pane.threadId);
      const r = result as GitActionResult;
      if (r.error) {
        pane.setError(`Pull failed: ${r.error}`);
      } else {
        addToast('success', 'Pulled successfully');
        await refreshStatus();
      }
    } catch (err) {
      pane.setError(`Pull failed: ${err}`);
    } finally {
      actionLoading = false;
    }
  }

  function handleCommitClose() {
    showCommit = false;
    refreshStatus();
  }
</script>

{#if status}
  <div class="relative flex">
    <button
      onclick={executePrimary}
      disabled={primaryAction.disabled || actionLoading}
      title={primaryAction.tooltip}
      class="text-xs px-2.5 py-1 rounded-l border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
    >
      {actionLoading ? '...' : primaryAction.label}
    </button>
    <button
      onclick={() => showDropdown = !showDropdown}
      title="More git actions"
      class="text-xs px-1 py-1 rounded-r border border-l-0 border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer"
    >
      <svg class="w-3 h-3" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5">
        <path d="M3 5l3 3 3-3" />
      </svg>
    </button>

    {#if showDropdown}
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <div class="fixed inset-0 z-40" onclick={() => showDropdown = false} onkeydown={(e) => { if (e.key === 'Escape') showDropdown = false; }}></div>
      <div class="absolute top-full right-0 mt-1 z-50 bg-surface-1 border border-border rounded shadow-lg min-w-[120px]">
        <button
          onclick={() => { showDropdown = false; showCommit = true; }}
          disabled={!status.hasChanges}
          class="w-full text-left px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 cursor-pointer disabled:opacity-40"
        >
          Commit
        </button>
        <button
          onclick={() => { showDropdown = false; doPush(); }}
          disabled={status.aheadCount === 0}
          class="w-full text-left px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 cursor-pointer disabled:opacity-40"
        >
          Push
        </button>
        <button
          onclick={() => { showDropdown = false; doPull(); }}
          disabled={status.behindCount === 0}
          class="w-full text-left px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 cursor-pointer disabled:opacity-40"
        >
          Pull
        </button>
      </div>
    {/if}
  </div>

  <CommitDialog {pane} open={showCommit} onClose={handleCommitClose} />
{/if}
