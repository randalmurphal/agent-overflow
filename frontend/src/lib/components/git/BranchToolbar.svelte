<script lang="ts">
  import { onMount } from 'svelte';
  import { fade, fly } from 'svelte/transition';
  import { GetGitStatus, GitListBranches, GitCheckout, GitCreateBranch } from '../../stores/bindings';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { GitBranch, GitStatus } from '../../types/git';

  let { pane }: { pane: ThreadPane } = $props();

  let currentBranch = $state('');
  let open = $state(false);
  let branches = $state<GitBranch[]>([]);
  let filter = $state('');
  let loading = $state(false);
  let showCreateInput = $state(false);
  let newBranchName = $state('');
  let creating = $state(false);

  let filteredBranches = $derived(
    filter
      ? branches.filter((b) => b.name.toLowerCase().includes(filter.toLowerCase()))
      : branches,
  );

  onMount(async () => {
    if (!pane.threadId) return;
    try {
      const status = await GetGitStatus(pane.threadId);
      if (status) {
        currentBranch = (status as GitStatus).branch;
      }
    } catch (err) {
      console.error('Failed to get git status:', err);
      pane.setError('Failed to load branch info');
    }
  });

  async function openPicker() {
    if (!pane.threadId) return;
    open = true;
    loading = true;
    try {
      const result = await GitListBranches(pane.threadId);
      branches = (result ?? []) as GitBranch[];
    } catch (err) {
      console.error('Failed to list branches:', err);
      pane.setError('Failed to list branches');
      branches = [];
    } finally {
      loading = false;
    }
  }

  async function selectBranch(name: string) {
    if (!pane.threadId || name === currentBranch) {
      open = false;
      return;
    }
    try {
      await GitCheckout(pane.threadId, name);
      currentBranch = name;
    } catch (err) {
      console.error('Failed to checkout branch:', err);
      pane.setError(`Failed to checkout: ${err}`);
    }
    open = false;
  }

  async function createBranch() {
    const name = newBranchName.trim();
    if (!name || !pane.threadId || creating) return;
    creating = true;
    try {
      await GitCreateBranch(pane.threadId, name);
      currentBranch = name;
      newBranchName = '';
      showCreateInput = false;
      open = false;
    } catch (err) {
      console.error('Failed to create branch:', err);
      pane.setError(`Failed to create branch: ${err}`);
    } finally {
      creating = false;
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      open = false;
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      open = false;
    }
  }
</script>

{#if currentBranch}
  <div class="relative">
    <button
      onclick={openPicker}
      aria-label="Switch branch: {currentBranch}"
      aria-expanded={open}
      aria-haspopup="listbox"
      class="flex items-center gap-1.5 text-xs px-2 py-1 rounded border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      <svg class="w-3 h-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
        <path d="M6 3v12M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM18 9a9 9 0 0 1-9 9" />
      </svg>
      <span class="truncate max-w-[180px]">{currentBranch}</span>
    </button>

    {#if open}
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <div transition:fade={{ duration: 100 }} class="fixed inset-0 z-40" onclick={handleBackdropClick} onkeydown={handleKeydown}></div>
      <div transition:fly={{ y: -4, duration: 120 }} class="absolute top-full left-0 mt-1 z-50 bg-surface-1 border border-border rounded-lg shadow-xl min-w-[220px] max-h-[300px] overflow-hidden flex flex-col">
        <div class="p-2 border-b border-border">
          <input
            type="text"
            bind:value={filter}
            placeholder="Filter branches..."
            aria-label="Filter branches"
            class="w-full text-xs rounded border border-border bg-surface-0 px-2 py-1 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
          />
        </div>
        <div class="overflow-y-auto flex-1" role="listbox" aria-label="Branches">
          {#if loading}
            <div class="px-3 py-2 text-xs text-text-secondary animate-pulse">Loading branches...</div>
          {:else}
            {#each filteredBranches as branch}
              <button
                onclick={() => selectBranch(branch.name)}
                role="option"
                aria-selected={branch.isCurrent}
                class="w-full text-left px-3 py-1.5 text-xs hover:bg-surface-2/50 cursor-pointer flex items-center gap-2
                  {branch.isCurrent ? 'text-accent font-medium' : 'text-text-secondary hover:text-text-primary'}"
              >
                <span class="truncate">{branch.name}</span>
                {#if branch.isRemote}
                  <span class="text-[9px] text-text-secondary/40 shrink-0">remote</span>
                {/if}
                {#if branch.isCurrent}
                  <span class="ml-auto text-accent shrink-0">&#10003;</span>
                {/if}
              </button>
            {/each}
            {#if filteredBranches.length === 0}
              <div class="px-3 py-2 text-xs text-text-secondary/60">No matching branches</div>
            {/if}
          {/if}
        </div>
        <div class="border-t border-border p-2">
          {#if showCreateInput}
            <div class="flex gap-1">
              <input
                type="text"
                bind:value={newBranchName}
                placeholder="new-branch-name"
                aria-label="New branch name"
                onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); createBranch(); } if (e.key === 'Escape') { showCreateInput = false; } }}
                class="flex-1 text-xs rounded border border-border bg-surface-0 px-2 py-1 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent min-w-0"
              />
              <button
                onclick={createBranch}
                disabled={!newBranchName.trim() || creating}
                class="text-xs px-2 py-1 rounded bg-accent text-surface-0 font-medium hover:opacity-90 disabled:opacity-40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
              >
                {creating ? '...' : 'Create'}
              </button>
            </div>
          {:else}
            <button
              onclick={() => showCreateInput = true}
              class="w-full text-left text-xs text-accent hover:text-accent/80 cursor-pointer px-1 py-0.5 rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              + Create new branch
            </button>
          {/if}
        </div>
      </div>
    {/if}
  </div>
{/if}
