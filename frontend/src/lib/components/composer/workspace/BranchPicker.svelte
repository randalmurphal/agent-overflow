<script lang="ts">
  // Branch trigger + list for the below-composer bar. Two modes share the
  // same dropdown surface:
  //
  //   - Selection: pick a branch to checkout (idle thread) or to use as
  //     the base for staged new-worktree intent.
  //   - Creation: top "+ New branch…" row toggles BranchCreateForm
  //     above the branch list; the branch list below becomes a base
  //     picker — selecting a row sets the form's base instead of
  //     switching the workspace.
  //
  // When the workspace is dirty, both modes surface a "Local (with
  // changes)" entry at the top of the branch list. Picking it carries
  // the uncommitted changes; picking any real branch while dirty
  // performs a clean checkout (the destructive path is gated by an
  // explicit confirmation chip in BranchCreateForm).

  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import GitBranchIcon from 'lucide-svelte/icons/git-branch';
  import Plus from 'lucide-svelte/icons/plus';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import Trash2 from 'lucide-svelte/icons/trash-2';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { GitBranch, GitStatus } from '../../../types/git';
  import type { Thread } from '../../../types/models';
  import {
    GetGitStatus,
    GetThread,
    GitCheckout,
    GitListBranches,
    GitMaybeFetchRemotes,
    GitPruneRemotes,
    GitSyncBranch,
    UpdateThreadWorkspace,
  } from '../../../stores/bindings';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { sameNormalizedPath } from '../../../utils/path';
  import {
    isLocalBase,
    LOCAL_BASE_SENTINEL,
    setWorktreeBaseBranch,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';
  import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import BranchCreateForm from './BranchCreateForm.svelte';

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
  let workspaceDirty = $state(false);
  let pruning = $state(false);
  let syncingBranch: string | null = $state(null);

  // BranchCreateForm owns its name + pending + error state. The parent
  // only tracks whether the form is mounted and what base the form is
  // pointed at — branch-row clicks flow back into createBase via $bindable.
  let creating = $state(false);
  let createBase = $state('');

  let currentBranch = $derived(pane.thread?.branch ?? '');
  let currentWorkspace = $derived(pane.thread?.workspacePath ?? '');
  let projectPath = $derived(pane.thread?.projectPath ?? '');
  let intent = $derived(worktreeIntentForThread(pane.thread));

  // The trigger surface reflects whatever the next action will use:
  //   - In worktree intent with the Local sentinel: "From Local (with changes)"
  //   - In worktree intent with a normal base: "From <base>"
  //   - Otherwise: the currently checked-out branch.
  let triggerLabel = $derived.by(() => {
    if (intent.mode !== 'new-worktree') return currentBranch || 'No branch';
    if (isLocalBase(intent.baseBranch)) return 'From Local (with changes)';
    const base = intent.baseBranch || currentBranch;
    return `From ${base || 'branch'}`;
  });

  let disabledReason = $derived(workspaceLock.reason);
  let workspaceChangingDisabled = $derived(workspaceLock.locked);

  let filteredBranches = $derived.by(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return branches;
    return branches.filter((branch) => branch.name.toLowerCase().includes(needle));
  });

  // Local-row visibility: only useful when we'd otherwise be choosing a
  // base (worktree intent or active create form). Surfacing it during
  // plain checkout selection would imply switching the current branch
  // would carry the changes — but plain checkout never does that.
  let showLocalRow = $derived(workspaceDirty && (intent.mode === 'new-worktree' || creating));

  // Highlight the Local row when the live worktree intent or the in-flight
  // create form is pointing at the sentinel. Both flows share the same row.
  let isLocalSelected = $derived.by(() => {
    if (creating) return isLocalBase(createBase);
    return intent.mode === 'new-worktree' && isLocalBase(intent.baseBranch);
  });

  async function handleTrigger(): Promise<void> {
    open = !open;
    if (!open) {
      cancelCreate();
      return;
    }
    if (!pane.thread || loading) return;
    const threadId = pane.thread.id;
    loading = true;
    // Two independent fetches, parallelized via Promise.all on IIFEs so
    // a synchronous failure in one (e.g. binding not wired in a test)
    // can't poison the other. workspaceDirty is a UI hint that drives
    // the "Local (with changes)" row; a failed dirty check should hide
    // the hint but never block the branch list render.
    const fetchBranches = (async () => {
      try {
        const res = (await GitListBranches(threadId)) as GitBranch[] | null;
        branches = Array.isArray(res) ? res : [];
      } catch (err) {
        console.error('GitListBranches failed:', err);
        branches = [];
      }
    })();
    const fetchStatus = (async () => {
      try {
        const status = (await GetGitStatus(threadId)) as GitStatus;
        workspaceDirty = !!status?.hasChanges;
      } catch (err) {
        console.error('GetGitStatus failed:', err);
        workspaceDirty = false;
      }
    })();
    // Stale-window fetch: backend returns immediately when fresh, runs
    // `git fetch --all` and returns true otherwise. Fires alongside the
    // initial list so the picker paints from cached refs instantly;
    // when the fetch lands we re-list to surface any new ahead/behind
    // counts or remote-only branches. Failures are silent — the
    // picker stays usable on stale data.
    void (async () => {
      try {
        const fetched = await GitMaybeFetchRemotes(threadId);
        if (!fetched) return;
        if (pane.thread?.id !== threadId || !open) return;
        const refreshed = (await GitListBranches(threadId)) as GitBranch[] | null;
        if (pane.thread?.id !== threadId || !open) return;
        if (Array.isArray(refreshed)) branches = refreshed;
      } catch (err) {
        console.error('background fetch failed:', err);
      }
    })();
    try {
      await Promise.all([fetchBranches, fetchStatus]);
    } finally {
      loading = false;
    }
  }

  // Sync surfaces on rows where local is behind upstream. The hard rule
  // is FF-only: diverged rows (ahead > 0 AND behind > 0) render the
  // icon disabled with a tooltip explaining why. The backend enforces
  // the same constraint via git's native refusal of non-FF refspecs,
  // so even a bypassed UI gate can't produce a non-FF update.
  function canSync(branch: GitBranch): boolean {
    return (branch.behindCount ?? 0) > 0 && (branch.aheadCount ?? 0) === 0;
  }

  function isDiverged(branch: GitBranch): boolean {
    return (branch.behindCount ?? 0) > 0 && (branch.aheadCount ?? 0) > 0;
  }

  function showsSyncAction(branch: GitBranch): boolean {
    return canSync(branch) || isDiverged(branch);
  }

  function syncDisabledTitle(branch: GitBranch): string | undefined {
    if (!isDiverged(branch)) return undefined;
    return `Branch has diverged from upstream (${branch.aheadCount} ahead, ${branch.behindCount} behind). Push or rebase first.`;
  }

  async function handleSync(branch: GitBranch): Promise<void> {
    if (!pane.thread || !canSync(branch)) return;
    if (syncingBranch) return;
    const threadId = pane.thread.id;
    syncingBranch = branch.name;
    try {
      const res = (await GitSyncBranch(threadId, branch.name)) as GitBranch[] | null;
      if (pane.thread?.id === threadId && Array.isArray(res)) {
        branches = res;
      }
      addToast('info', `Synced ${branch.name}`);
    } catch (err) {
      addToast('error', `Sync failed: ${errString(err)}`);
    } finally {
      syncingBranch = null;
    }
  }

  async function handlePrune(): Promise<void> {
    if (!pane.thread || pruning) return;
    const threadId = pane.thread.id;
    pruning = true;
    try {
      const res = (await GitPruneRemotes(threadId)) as GitBranch[] | null;
      if (pane.thread?.id === threadId && Array.isArray(res)) {
        branches = res;
      }
      addToast('info', 'Pruned stale remote branches');
    } catch (err) {
      addToast('error', `Prune failed: ${errString(err)}`);
    } finally {
      pruning = false;
    }
  }

  function closeMenu(): void {
    open = false;
    query = '';
    cancelCreate();
    triggerEl?.focus();
  }

  // The suffix slot on a branch row carries (1) ahead/behind hints
  // versus upstream and (2) the branch tag (worktree / default).
  // When both are present they're joined with a thin separator so the
  // arrows read as one group and the tag as another.
  function branchBadge(branch: GitBranch): string | undefined {
    const counts: string[] = [];
    if ((branch.aheadCount ?? 0) > 0) counts.push(`↑${branch.aheadCount}`);
    if ((branch.behindCount ?? 0) > 0) counts.push(`↓${branch.behindCount}`);
    let tag: string | undefined;
    if (branch.worktreePath && !sameNormalizedPath(branch.worktreePath, currentWorkspace)) {
      tag = 'worktree';
    } else if (branch.isDefault) {
      tag = 'default';
    }
    if (counts.length === 0) return tag;
    const arrows = counts.join(' ');
    return tag ? `${arrows} · ${tag}` : arrows;
  }

  function isSelectedBranch(branch: GitBranch): boolean {
    if (branch.name !== currentBranch) return false;
    if (branch.worktreePath) return sameNormalizedPath(branch.worktreePath, currentWorkspace);
    return true;
  }

  // Highlight the row that backs the active flow's base. In creation
  // mode this is the form's createBase; in worktree-intent mode it's
  // the staged baseBranch; otherwise it's the checkout state.
  function isBaseSelected(branch: GitBranch): boolean {
    if (creating) return branch.name === createBase;
    if (intent.mode === 'new-worktree') return branch.name === intent.baseBranch;
    return isSelectedBranch(branch);
  }

  function handleSearchKeydown(event: KeyboardEvent): void {
    event.stopPropagation();
  }

  function startCreate(): void {
    if (!pane.thread || workspaceChangingDisabled) return;
    // Default base mirrors the worktree intent flow: dirty workspace
    // pre-selects "Local (with changes)" so the destructive clean-checkout
    // path is opt-in. Otherwise, branch off the current HEAD.
    createBase = workspaceDirty ? LOCAL_BASE_SENTINEL : currentBranch;
    creating = true;
  }

  function cancelCreate(): void {
    creating = false;
    createBase = '';
  }

  function handleCreated(updated: Thread): void {
    syncThread(updated);
    addToast('info', 'Created branch');
    cancelCreate();
    closeMenu();
  }

  function selectLocalRow(): void {
    if (!pane.thread) return;
    if (creating) {
      createBase = LOCAL_BASE_SENTINEL;
      return;
    }
    if (intent.mode === 'new-worktree' && !workspaceChangingDisabled) {
      // setWorktreeBaseBranch flips carryLocalChanges based on the
      // sentinel — the store keeps the carry flag in sync, the picker
      // doesn't need to repeat that logic.
      setWorktreeBaseBranch(pane.thread, LOCAL_BASE_SENTINEL);
      closeMenu();
    }
  }

  async function selectBranch(branch: GitBranch): Promise<void> {
    if (creating) {
      createBase = branch.name;
      return;
    }
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

{#snippet syncIcon()}
  <Icon icon={RefreshCw} size={12} strokeWidth={2} />
{/snippet}

<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread || applying}
  aria-haspopup="menu"
  aria-expanded={open}
  data-testid="branch-picker-trigger"
  class={composerTriggerClasses}
>
  <Icon icon={GitBranchIcon} size={12} strokeWidth={2} class="opacity-70" />
  <span class="truncate max-w-[160px] text-fg">{triggerLabel}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>

<Popover
  anchor={triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-end"
  role="none"
>
  <Menu ariaLabel="Branches" onClose={closeMenu}>
    {#if creating && pane.thread}
      <BranchCreateForm
        {pane}
        {workspaceDirty}
        {currentBranch}
        bind:base={createBase}
        onCancel={cancelCreate}
        onCreated={handleCreated}
      />
      <MenuDivider />
    {:else}
      <MenuItem
        label="New branch…"
        disabled={workspaceChangingDisabled}
        title={workspaceChangingDisabled ? disabledReason : undefined}
        onSelect={startCreate}
      >
        {#snippet icon()}
          <Icon icon={Plus} size={12} strokeWidth={2} />
        {/snippet}
      </MenuItem>
      <MenuItem
        label={pruning ? 'Pruning…' : 'Prune stale branches'}
        disabled={pruning}
        onSelect={handlePrune}
      >
        {#snippet icon()}
          <Icon icon={Trash2} size={12} strokeWidth={2} />
        {/snippet}
      </MenuItem>
      <MenuDivider />
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
    {/if}
    {#if loading}
      <div
        class="px-3 py-1.5 text-xs text-text-secondary/60"
        role="presentation"
        data-testid="branch-picker-loading"
      >
        Loading branches…
      </div>
    {:else}
      {#if showLocalRow}
        <MenuItem
          label="Local (with changes)"
          description={currentBranch || undefined}
          checked={isLocalSelected}
          disabled={!creating && workspaceChangingDisabled}
          title={!creating && workspaceChangingDisabled ? disabledReason : undefined}
          onSelect={selectLocalRow}
        />
        <MenuDivider />
      {/if}
      {#if filteredBranches.length === 0}
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
              checked={isBaseSelected(branch)}
              disabled={!creating && workspaceChangingDisabled}
              title={!creating && workspaceChangingDisabled ? disabledReason : undefined}
              onSelect={() => selectBranch(branch)}
              action={showsSyncAction(branch) ? syncIcon : undefined}
              actionLabel={syncingBranch === branch.name
                ? `Syncing ${branch.name}`
                : `Sync ${branch.name} from upstream`}
              actionDisabled={!canSync(branch) || syncingBranch !== null}
              actionTitle={syncDisabledTitle(branch)}
              onAction={() => handleSync(branch)}
            />
          {/each}
        </div>
      {/if}
    {/if}
  </Menu>
</Popover>
