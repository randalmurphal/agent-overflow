<script lang="ts">
  // Branch trigger + list for the below-composer bar. Replaces the
  // old git/BranchToolbar's trigger + listbox with the Menu primitive;
  // creation of new branches remains available through the Git Actions
  // command palette entries, so this picker stays scoped to selection.

  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import GitBranchIcon from 'lucide-svelte/icons/git-branch';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { GitBranch } from '../../../types/git';
  import type { Thread } from '../../../types/models';
  import {
    GetThread,
    GitCheckout,
    GitListBranches,
    UpdateThreadWorkspace,
  } from '../../../stores/bindings';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { sameNormalizedPath } from '../../../utils/path';
  import {
    setWorktreeBaseBranch,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';
  import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';

  interface Props {
    pane: ThreadPane;
    workspaceLock: WorkspaceChangeLockState;
  }

  let { pane, workspaceLock }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let branches: GitBranch[] = $state([]);
  let query = $state('');
  let loading = $state(false);
  let applying = $state(false);

  let currentBranch = $derived(pane.thread?.branch ?? '');
  let currentWorkspace = $derived(pane.thread?.workspacePath ?? '');
  let projectPath = $derived(pane.thread?.projectPath ?? '');
  let intent = $derived(worktreeIntentForThread(pane.thread));
  let triggerBranch = $derived(intent.mode === 'new-worktree' ? intent.baseBranch || currentBranch : currentBranch);
  let triggerLabel = $derived(intent.mode === 'new-worktree' ? `From ${triggerBranch || 'branch'}` : currentBranch || 'No branch');
  let disabledReason = $derived(workspaceLock.reason);
  let workspaceChangingDisabled = $derived(workspaceLock.locked);
  let filteredBranches = $derived.by(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return branches;
    return branches.filter((branch) => branch.name.toLowerCase().includes(needle));
  });

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
    query = '';
    triggerEl?.focus();
  }

  function branchBadge(branch: GitBranch): string | undefined {
    if (branch.worktreePath && !sameNormalizedPath(branch.worktreePath, currentWorkspace)) return 'worktree';
    if (branch.isDefault) return 'default';
    if (branch.isRemote) return 'remote';
    return undefined;
  }

  function isSelectedBranch(branch: GitBranch): boolean {
    if (branch.name !== currentBranch) return false;
    if (branch.worktreePath) return sameNormalizedPath(branch.worktreePath, currentWorkspace);
    return true;
  }

  function handleSearchKeydown(event: KeyboardEvent): void {
    event.stopPropagation();
  }

  async function selectBranch(branch: GitBranch): Promise<void> {
    if (!pane.thread || applying) {
      closeMenu();
      return;
    }
    if (workspaceChangingDisabled) {
      closeMenu();
      return;
    }
    if (intent.mode === 'new-worktree') {
      setWorktreeBaseBranch(pane.thread, branch.name);
      closeMenu();
      return;
    }
    if (isSelectedBranch(branch)) {
      closeMenu();
      return;
    }
    applying = true;
    try {
      let refreshed: Thread | null;
      const shouldReturnToProjectRoot =
        branch.isDefault &&
        projectPath &&
        !sameNormalizedPath(currentWorkspace, projectPath) &&
        (!branch.worktreePath || sameNormalizedPath(branch.worktreePath, projectPath));

      if (shouldReturnToProjectRoot) {
        await GitCheckout(pane.thread.id, branch.name);
        refreshed = (await GetThread(pane.thread.id)) as Thread | null;
      } else if (branch.worktreePath && !sameNormalizedPath(branch.worktreePath, currentWorkspace)) {
        refreshed = (await UpdateThreadWorkspace(pane.thread.id, branch.worktreePath)) as Thread;
      } else {
        await GitCheckout(pane.thread.id, branch.name);
        refreshed = (await GetThread(pane.thread.id)) as Thread | null;
      }
      if (refreshed) {
        syncThread(refreshed);
      }
      addToast('info', `Checked out ${branch.name}`);
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
  <GitBranchIcon class="h-3 w-3" aria-hidden="true" />
  <span class="truncate max-w-[160px]">{triggerLabel}</span>
  <ChevronDown class="h-3 w-3" aria-hidden="true" />
</button>

<Popover
  anchor={triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-end"
  role="none"
>
  <Menu ariaLabel="Branches" onClose={closeMenu}>
    <div class="px-2 pb-1">
      <input
        type="search"
        value={query}
        placeholder="Search Branches"
        onkeydown={handleSearchKeydown}
        oninput={(e) => (query = (e.target as HTMLInputElement).value)}
        class={[
          'h-7 w-72 rounded border border-border-subtle bg-surface-0',
          'px-2 text-xs text-text-primary placeholder:text-fg-hint',
          'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        ].join(' ')}
      />
    </div>
    {#if loading}
      <div
        class="px-3 py-1.5 text-xs text-text-secondary/60"
        role="presentation"
        data-testid="branch-picker-loading"
      >
        Loading branches…
      </div>
    {:else if filteredBranches.length === 0}
      <div
        class="px-3 py-1.5 text-xs text-text-secondary/60"
        role="presentation"
        data-testid="branch-picker-empty"
      >
      No branches
      </div>
    {:else}
      <div class="max-h-56 overflow-y-auto">
      {#each filteredBranches as branch (branch.name)}
        <MenuItem
          label={branch.name}
          suffix={branchBadge(branch)}
          checked={intent.mode === 'new-worktree' ? branch.name === triggerBranch : isSelectedBranch(branch)}
          disabled={workspaceChangingDisabled}
          title={workspaceChangingDisabled ? disabledReason : undefined}
          onSelect={() => selectBranch(branch)}
        />
      {/each}
      </div>
    {/if}
  </Menu>
</Popover>
