<script lang="ts">
  // Branch trigger + list for the below-composer bar. Replaces the
  // old git/BranchToolbar's trigger + listbox with the Menu primitive;
  // creation of new branches remains available through the Git Actions
  // command palette entries, so this picker stays scoped to selection.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { GitBranch } from '../../../types/git';
  import type { Thread } from '../../../types/models';
  import {
    GetThread,
    GitCheckout,
    GitListBranches,
    UpdateThreadBranch,
  } from '../../../stores/bindings';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let branches: GitBranch[] = $state([]);
  let loading = $state(false);
  let applying = $state(false);

  let currentBranch = $derived(pane.thread?.branch ?? '');

  async function handleTrigger(): Promise<void> {
    open = !open;
    if (!open || !pane.thread || loading || branches.length > 0) return;
    loading = true;
    try {
      const res = (await GitListBranches(pane.thread.id)) as GitBranch[] | null;
      branches = Array.isArray(res) ? res : [];
    } catch (err) {
      console.error('GitListBranches failed:', err);
      branches = [];
    } finally {
      loading = false;
    }
  }

  function closeMenu(): void {
    open = false;
    triggerEl?.focus();
  }

  async function selectBranch(name: string): Promise<void> {
    if (!pane.thread || applying || name === currentBranch) {
      closeMenu();
      return;
    }
    applying = true;
    try {
      await GitCheckout(pane.thread.id, name);
      await UpdateThreadBranch(pane.thread.id, name);
      // Refresh the thread so the sidebar and the trigger both reflect
      // the checkout — UpdateThreadBranch already returns the updated
      // row, but GetThread is the single-source read after a filesystem
      // checkout so we pick up any other branch-driven fields too.
      const refreshed = (await GetThread(pane.thread.id)) as Thread | null;
      if (refreshed) {
        pane.replaceThread(refreshed);
        replaceThread(refreshed);
      }
      addToast('info', `Checked out ${name}`);
    } catch (err) {
      console.error('branch checkout failed:', err);
      addToast('error', `Failed to checkout: ${errString(err)}`);
    } finally {
      applying = false;
      closeMenu();
    }
  }
</script>

<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread || applying}
  aria-haspopup="menu"
  aria-expanded={open}
  data-testid="branch-picker-trigger"
  class={[
    'inline-flex items-center gap-1 rounded border border-border',
    'px-2 py-0.5 text-[11px] text-text-secondary',
    'transition-colors cursor-pointer',
    'hover:border-text-secondary hover:text-text-primary',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <svg
    viewBox="0 0 24 24"
    class="h-3 w-3"
    fill="none"
    stroke="currentColor"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path d="M6 3v12M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM18 9a9 9 0 0 1-9 9" />
  </svg>
  <span class="truncate max-w-[160px]">{currentBranch || 'No branch'}</span>
  <svg
    viewBox="0 0 24 24"
    class="h-3 w-3"
    fill="none"
    stroke="currentColor"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path d="M6 9l6 6 6-6" />
  </svg>
</button>

<Popover
  anchor={triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-end"
  role="none"
>
  <Menu ariaLabel="Branches" onClose={closeMenu}>
    {#if loading}
      <div
        class="px-3 py-1.5 text-xs text-text-secondary/60"
        role="presentation"
        data-testid="branch-picker-loading"
      >
        Loading branches…
      </div>
    {:else if branches.length === 0}
      <div
        class="px-3 py-1.5 text-xs text-text-secondary/60"
        role="presentation"
        data-testid="branch-picker-empty"
      >
        No branches
      </div>
    {:else}
      {#each branches as branch (branch.name)}
        <MenuItem
          label={branch.name}
          checked={branch.isCurrent || branch.name === currentBranch}
          onSelect={() => selectBranch(branch.name)}
        />
      {/each}
    {/if}
  </Menu>
</Popover>
