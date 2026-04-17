<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { fade, fly } from 'svelte/transition';
  import { GetGitStatus, GetThread, GitPush, GitPull, GitCreatePR, GitRemoveWorktree } from '../../stores/bindings';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { replaceThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { Thread } from '../../types/models';
  import type { GitActionResult, GitStatus } from '../../types/git';
  import CommitDialog from './CommitDialog.svelte';
  import ShipChangesDrawer from './ShipChangesDrawer.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let status = $state<GitStatus | null>(null);
  let statusError = $state(false);
  let actionLoading = $state(false);
  let showCommit = $state(false);
  let showShip = $state(false);
  let showDropdown = $state(false);
  let showRemoveWorktreeConfirm = $state(false);

  // The command palette fires this window event so a keyboard shortcut can
  // launch the wizard without threading a callback through every parent.
  function handleOpenShip(): void {
    if (pane.threadId) showShip = true;
  }

  onMount(() => {
    window.addEventListener('agent-overflow:open-ship-changes', handleOpenShip);
  });

  onDestroy(() => {
    if (typeof window !== 'undefined') {
      window.removeEventListener('agent-overflow:open-ship-changes', handleOpenShip);
    }
  });
  let menuTriggerEl: HTMLButtonElement | undefined = $state(undefined);
  let menuEl: HTMLDivElement | undefined = $state(undefined);
  let statusLoadGeneration = 0;

  let isWorktree = $derived(!!pane.thread?.worktreePath);
  let canCreatePR = $derived(
    status !== null && status.hasUpstream && !status.openPrUrl && !status.isDefaultBranch
  );

  async function refreshStatus(threadId: string | null = pane.threadId) {
    if (!threadId) return;
    const generation = ++statusLoadGeneration;
    try {
      const result = await GetGitStatus(threadId);
      if (generation !== statusLoadGeneration) return;
      status = result as GitStatus;
      statusError = false;
    } catch (err) {
      if (generation !== statusLoadGeneration) return;
      console.error('Failed to get git status:', err);
      status = null;
      statusError = true;
      pane.setError(`Failed to load git status: ${err}`);
    }
  }

  $effect(() => {
    const threadId = pane.threadId;
    status = null;
    statusError = false;
    showDropdown = false;
    showCommit = false;
    showRemoveWorktreeConfirm = false;

    if (!threadId) {
      return;
    }

    void refreshStatus(threadId);
  });

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
        console.error('Push failed:', r.error);
        pane.setError(`Push failed: ${r.error}`);
      } else {
        addToast('success', 'Pushed successfully');
        await refreshStatus();
      }
    } catch (err) {
      console.error('Push failed:', err);
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
        console.error('Pull failed:', r.error);
        pane.setError(`Pull failed: ${r.error}`);
      } else {
        addToast('success', 'Pulled successfully');
        await refreshStatus();
      }
    } catch (err) {
      console.error('Pull failed:', err);
      pane.setError(`Pull failed: ${err}`);
    } finally {
      actionLoading = false;
    }
  }

  async function doCreatePR() {
    if (!pane.threadId || actionLoading) return;
    actionLoading = true;
    try {
      const result = await GitCreatePR(pane.threadId, '', '');
      const r = result as GitActionResult;
      if (r.error) {
        console.error('Create PR failed:', r.error);
        pane.setError(`Create PR failed: ${r.error}`);
      } else {
        const msg = r.prUrl ? `PR created: ${r.prUrl}` : 'PR created';
        addToast('success', msg);
        await refreshStatus();
      }
    } catch (err) {
      console.error('Create PR failed:', err);
      pane.setError(`Create PR failed: ${err}`);
    } finally {
      actionLoading = false;
    }
  }

  async function doRemoveWorktree() {
    if (!pane.threadId || actionLoading) return;
    actionLoading = true;
    try {
      await GitRemoveWorktree(pane.threadId);
      const refreshedThread = await GetThread(pane.threadId) as Thread;
      pane.replaceThread(refreshedThread);
      replaceThread(refreshedThread);
      addToast('success', 'Worktree removed');
      await refreshStatus();
    } catch (err) {
      console.error('Remove worktree failed:', err);
      pane.setError(`Remove worktree failed: ${err}`);
    } finally {
      actionLoading = false;
    }
  }

  // Focus the first enabled menuitem when dropdown opens
  $effect(() => {
    if (showDropdown && menuEl) {
      const first = menuEl.querySelector<HTMLElement>('button[role="menuitem"]:not([disabled])');
      first?.focus();
    }
  });

  function handleMenuKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      showDropdown = false;
      menuTriggerEl?.focus();
      return;
    }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!menuEl) return;
      const items = [...menuEl.querySelectorAll<HTMLElement>('button[role="menuitem"]:not([disabled])')];
      if (items.length === 0) return;
      const current = items.indexOf(document.activeElement as HTMLElement);
      const next = e.key === 'ArrowDown'
        ? (current + 1) % items.length
        : (current - 1 + items.length) % items.length;
      items[next].focus();
    }
  }

  function handleCommitClose() {
    showCommit = false;
    refreshStatus();
  }
</script>

{#if statusError}
  <button
    onclick={() => refreshStatus()}
    class="text-xs px-2 py-1 rounded border border-error/40 text-error/80 hover:text-error cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    title="Failed to load git status. Click to retry."
  >
    Git: error
  </button>
{:else if status}
  <div class="relative flex">
    <button
      onclick={executePrimary}
      disabled={primaryAction.disabled || actionLoading}
      title={primaryAction.tooltip}
      class="text-xs px-2.5 py-1 rounded-l border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      {actionLoading ? '...' : primaryAction.label}
    </button>
    <button
      bind:this={menuTriggerEl}
      onclick={() => showDropdown = !showDropdown}
      aria-label="More git actions"
      aria-expanded={showDropdown}
      aria-haspopup="menu"
      class="text-xs px-1 py-1 rounded-r border border-l-0 border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      <svg class="w-3 h-3" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true">
        <path d="M3 5l3 3 3-3" />
      </svg>
    </button>

    {#if showDropdown}
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <div transition:fade={{ duration: 100 }} class="fixed inset-0 z-40" onclick={() => { showDropdown = false; menuTriggerEl?.focus(); }} onkeydown={(e) => { if (e.key === 'Escape') { showDropdown = false; menuTriggerEl?.focus(); } }}></div>
      <!-- svelte-ignore a11y_interactive_supports_focus -->
      <div bind:this={menuEl} onkeydown={handleMenuKeydown} transition:fly={{ y: -4, duration: 120 }} class="absolute top-full right-0 mt-1 z-50 bg-surface-1 border border-border rounded-lg shadow-lg min-w-[140px]" role="menu" aria-label="Git actions">
        <button
          onclick={() => { showDropdown = false; showCommit = true; }}
          disabled={!status.hasChanges}
          role="menuitem"
          class="w-full text-left px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 active:bg-surface-2/70 cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          Commit
        </button>
        <button
          onclick={() => { showDropdown = false; doPush(); }}
          disabled={status.aheadCount === 0}
          role="menuitem"
          class="w-full text-left px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 active:bg-surface-2/70 cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          Push
        </button>
        <button
          onclick={() => { showDropdown = false; doPull(); }}
          disabled={status.behindCount === 0}
          role="menuitem"
          class="w-full text-left px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 active:bg-surface-2/70 cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          Pull
        </button>
        <button
          onclick={() => { showDropdown = false; doCreatePR(); }}
          disabled={!canCreatePR}
          role="menuitem"
          class="w-full text-left px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 active:bg-surface-2/70 cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          Create PR
        </button>
        <div class="border-t border-border my-0.5"></div>
        <button
          onclick={() => { showDropdown = false; showShip = true; }}
          role="menuitem"
          data-testid="git-actions-ship"
          class="w-full text-left px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 active:bg-surface-2/70 cursor-pointer transition-colors"
        >
          Ship Changes…
        </button>
        {#if isWorktree}
          <div class="border-t border-border my-0.5"></div>
          <button
            onclick={() => { showDropdown = false; showRemoveWorktreeConfirm = true; }}
            role="menuitem"
            class="w-full text-left px-3 py-1.5 text-xs text-error/80 hover:text-error hover:bg-surface-2/50 active:bg-surface-2/70 cursor-pointer transition-colors"
          >
            Remove Worktree
          </button>
        {/if}
      </div>
    {/if}
  </div>

  <CommitDialog {pane} open={showCommit} onClose={handleCommitClose} />

  <ShipChangesDrawer
    {pane}
    open={showShip}
    onClose={() => { showShip = false; refreshStatus(); }}
  />

  <ConfirmDialog
    open={showRemoveWorktreeConfirm}
    title="Remove worktree"
    description="This will remove the git worktree for this thread. The branch will be preserved but the working directory will be deleted."
    confirmLabel="Remove"
    destructive={true}
    onConfirm={() => { showRemoveWorktreeConfirm = false; doRemoveWorktree(); }}
    onCancel={() => { showRemoveWorktreeConfirm = false; }}
  />
{/if}
