<script lang="ts">
  // Branch trigger + list for the below-composer bar. The trigger
  // doubles as a "from <base>" picker whenever the worktree intent
  // has the creating-branch flag set: in that quadrant the rows are a
  // base picker, not a checkout target. Otherwise behavior splits by
  // workspace mode:
  //
  //   - mode='local':         pick a row → checkout that branch
  //   - mode='new-worktree':  pick a row → stage as the worktree's
  //     attach target. If the branch already has a worktree, flip
  //     mode='local' and switch workspace to the existing one (git's
  //     own one-branch-one-worktree invariant — no point staging an
  //     attach that would fail at materialization).
  //
  // The "+ New branch…" entry inside the dropdown is the local-mode
  // entry point into the create-branch flow; the inline "+ new branch"
  // button next to BranchPicker (in WorktreeNameInput.svelte) is the
  // new-worktree-mode entry point. Both call enterCreateBranchMode and
  // surface the same WorktreeNameInput text input above the strip.

  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import GitBranchIcon from 'lucide-svelte/icons/git-branch';
  import Plus from 'lucide-svelte/icons/plus';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import Trash2 from 'lucide-svelte/icons/trash-2';
  import X from 'lucide-svelte/icons/x';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { GitBranch, GitStatus } from '../../../types/git';
  import type { Thread } from '../../../types/models';
  import {
    GetGitStatus,
    GetGitStatusForProject,
    GetThread,
    GitCheckout,
    GitListBranches,
    GitListBranchesForProject,
    GitMaybeFetchRemotes,
    GitMaybeFetchRemotesForProject,
    GitPruneRemotes,
    GitSyncBranch,
    UpdateThreadWorkspace,
  } from '../../../stores/bindings';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { sameNormalizedPath } from '../../../utils/path';
  import {
    enterCreateBranchMode,
    exitCreateBranchMode,
    isLocalBase,
    LOCAL_BASE_SENTINEL,
    setAttachBranch,
    setNewBranchBase,
    setThreadEnvMode,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';
  import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { focusPaneComposer } from '../../panes/paneComposerFocus';

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
  let lastOpenBranchKey = $state('');
  let branchRefreshSeq = 0;

  let currentBranch = $derived(pane.thread?.branch ?? '');
  let currentWorkspace = $derived(pane.thread?.workspacePath ?? '');
  let projectPath = $derived(pane.thread?.projectPath ?? '');
  let intent = $derived(worktreeIntentForThread(pane.thread));

  // Trigger label reflects what picking a row will do next:
  //   - creatingBranch:        "From <base>" (rows set the base)
  //   - new-worktree, !creating: <attach branch | currentBranch> (rows attach)
  //   - local, !creating:      currentBranch (rows checkout)
  let triggerLabel = $derived.by(() => {
    if (intent.creatingBranch) {
      if (isLocalBase(intent.newBranchBase)) return 'From Local (with changes)';
      const base = intent.newBranchBase || currentBranch;
      return `From ${base || 'branch'}`;
    }
    if (intent.mode === 'new-worktree') {
      return intent.attachBranch || currentBranch || 'No branch';
    }
    return currentBranch || 'No branch';
  });

  let workspaceChangingDisabled = $derived(workspaceLock.locked);

  function orderBranchesForDisplay(sourceBranches: GitBranch[]): GitBranch[] {
    return sourceBranches
      .map((branch, index) => ({ branch, index }))
      .sort((left, right) => {
        if (left.branch.isDefault !== right.branch.isDefault) {
          return left.branch.isDefault ? -1 : 1;
        }
        return left.index - right.index;
      })
      .map(({ branch }) => branch);
  }

  let orderedBranches = $derived(orderBranchesForDisplay(branches));

  let filteredBranches = $derived.by(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return orderedBranches;
    return orderedBranches.filter((branch) => branch.name.toLowerCase().includes(needle));
  });

  // Local-row only makes sense as a base picker — the user is choosing
  // what their NEW branch is built off. Outside the create flow it would
  // suggest "checkout the local working copy", which isn't a thing.
  let showLocalRow = $derived(workspaceDirty && intent.creatingBranch);

  let isLocalSelected = $derived(intent.creatingBranch && isLocalBase(intent.newBranchBase));

  function branchRefreshKey(threadIdentity: string | undefined, branch: string): string {
    return `${threadIdentity ?? ''}\0${branch}`;
  }

  async function refreshBranches(threadIdentity: string): Promise<void> {
    const seq = ++branchRefreshSeq;
    try {
      const res = pane.threadId
        ? (await GitListBranches(pane.threadId)) as GitBranch[] | null
        : pane.thread?.projectId
          ? (await GitListBranchesForProject(pane.thread.projectId)) as GitBranch[] | null
          : [];
      if (seq !== branchRefreshSeq) return;
      if (pane.thread?.id !== threadIdentity || !open) return;
      branches = Array.isArray(res) ? res : [];
    } catch (err) {
      console.error('GitListBranches failed:', err);
      if (seq !== branchRefreshSeq) return;
      if (pane.thread?.id === threadIdentity && open) branches = [];
    }
  }

  async function handleTrigger(): Promise<void> {
    open = !open;
    if (!open) return;
    if (!pane.thread || loading) return;
    const projectId = pane.thread.projectId;
    if (!pane.threadId && !projectId) return;
    const threadIdentity = pane.thread.id;
    lastOpenBranchKey = branchRefreshKey(threadIdentity, currentBranch);
    loading = true;
    const fetchBranches = refreshBranches(threadIdentity);
    const fetchStatus = (async () => {
      try {
        let status: GitStatus;
        if (pane.threadId) {
          status = (await GetGitStatus(pane.threadId)) as GitStatus;
        } else {
          if (!projectId) return;
          status = (await GetGitStatusForProject(projectId)) as GitStatus;
        }
        workspaceDirty = !!status?.hasChanges;
      } catch (err) {
        console.error('GetGitStatus failed:', err);
        workspaceDirty = false;
      }
    })();
    void (async () => {
      try {
        let fetched: boolean;
        if (pane.threadId) {
          fetched = await GitMaybeFetchRemotes(pane.threadId);
        } else {
          if (!projectId) return;
          fetched = await GitMaybeFetchRemotesForProject(projectId);
        }
        if (!fetched) return;
        if (pane.thread?.id !== threadIdentity || !open) return;
        await refreshBranches(threadIdentity);
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

  $effect(() => {
    const branch = currentBranch;
    const threadIdentity = pane.thread?.id;
    const key = branchRefreshKey(threadIdentity, branch);
    if (!open || !threadIdentity) {
      lastOpenBranchKey = key;
      return;
    }
    if (key === lastOpenBranchKey) return;
    lastOpenBranchKey = key;
    void refreshBranches(threadIdentity);
  });

  // FF-only sync. Diverged rows render the icon disabled with a
  // tooltip; backend mirrors the gate via git's native non-FF refusal.
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
    if (!pane.thread || !pane.threadId || !canSync(branch)) return;
    if (syncingBranch) return;
    const threadId = pane.threadId;
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
    if (!pane.thread || !pane.threadId || pruning) return;
    const threadId = pane.threadId;
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
    // Composer-toolbar pickers sit just under the textarea; after the
    // menu closes the user is almost always going to keep typing. Send
    // focus back to the textarea so Enter / Esc / chord-toggle don't
    // strand them on a trigger button. `focusPaneComposer` is a no-op
    // if the textarea is gone (pane unmounted, thread cleared).
    if (!focusPaneComposer(pane.paneId)) triggerEl?.focus();
  }

  $effect(() => {
    return registerComposerPicker(pane.paneId, 'branch', {
      isOpen: () => open,
      open: () => {
        if (open || !pane.thread) return;
        // Reuse handleTrigger so the open path runs the same fetch
        // pipeline as a mouse click on the trigger button.
        void handleTrigger();
      },
      close: () => {
        if (!open) return;
        closeMenu();
      },
    });
  });

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

  // Cap visible branch labels at 20 chars (19 + ellipsis); full name
  // surfaces via the row's title attribute.
  const BRANCH_LABEL_MAX_CHARS = 20;

  function truncateBranchLabel(name: string): string {
    if (name.length <= BRANCH_LABEL_MAX_CHARS) return name;
    return name.slice(0, BRANCH_LABEL_MAX_CHARS - 1) + '…';
  }

  function branchRowTitle(branch: GitBranch): string | undefined {
    if (branch.name.length > BRANCH_LABEL_MAX_CHARS) return branch.name;
    return undefined;
  }

  function isSelectedBranch(branch: GitBranch): boolean {
    if (branch.name !== currentBranch) return false;
    if (branch.worktreePath) return sameNormalizedPath(branch.worktreePath, currentWorkspace);
    return true;
  }

  // Highlight reflects the active flow's target:
  //   - creating: the row that is the staged base
  //   - new-worktree, !creating: the row that is the staged attach target
  //   - local, !creating: the row that's currently checked out
  function isBaseSelected(branch: GitBranch): boolean {
    if (intent.creatingBranch) return branch.name === intent.newBranchBase;
    if (intent.mode === 'new-worktree') return branch.name === intent.attachBranch;
    return isSelectedBranch(branch);
  }

  function handleSearchKeydown(event: KeyboardEvent): void {
    event.stopPropagation();
  }

  function startCreate(): void {
    if (!pane.thread) return;
    enterCreateBranchMode(pane.thread, { workspaceDirty, currentBranch });
    closeMenu();
  }

  function cancelCreate(): void {
    if (!pane.thread) return;
    exitCreateBranchMode(pane.thread);
    closeMenu();
  }

  function selectLocalRow(): void {
    if (!pane.thread) return;
    if (!intent.creatingBranch) return;
    setNewBranchBase(pane.thread, LOCAL_BASE_SENTINEL);
    closeMenu();
  }

  async function selectBranch(branch: GitBranch): Promise<void> {
    if (!pane.thread) {
      closeMenu();
      return;
    }
    if (intent.creatingBranch) {
      setNewBranchBase(pane.thread, branch.name);
      closeMenu();
      return;
    }
    if (applying) {
      closeMenu();
      return;
    }
    if (intent.mode === 'new-worktree') {
      // Dedup: a branch can only have one worktree at a time. If one
      // already exists, switch to it instead of staging an attach that
      // would fail at materialization.
      if (
        branch.worktreePath &&
        !sameNormalizedPath(branch.worktreePath, currentWorkspace)
      ) {
        if (workspaceChangingDisabled) {
          addToast('error', workspaceLock.reason);
          closeMenu();
          return;
        }
        setThreadEnvMode(pane.thread, 'local');
        if (pane.hasDraftPlaceholder) {
          pane.applyDraftPlaceholderWorkspace({
            workspacePath: branch.worktreePath,
            worktreePath: branch.worktreePath,
            branch: branch.name,
          });
          addToast('info', `Selected existing worktree for ${branch.name}`);
          closeMenu();
          return;
        }
        applying = true;
        try {
          const updated = (await UpdateThreadWorkspace(
            pane.thread.id,
            branch.worktreePath,
          )) as Thread;
          syncThread(updated);
          addToast('info', `Switched to existing worktree for ${branch.name}`);
        } catch (err) {
          console.error('UpdateThreadWorkspace failed:', err);
          addToast('error', `Failed to switch worktree: ${errString(err)}`);
        } finally {
          applying = false;
          closeMenu();
        }
        return;
      }
      setAttachBranch(pane.thread, branch.name);
      closeMenu();
      return;
    }
    if (isSelectedBranch(branch)) {
      closeMenu();
      return;
    }
    if (pane.hasDraftPlaceholder) {
      if (branch.worktreePath && !sameNormalizedPath(branch.worktreePath, currentWorkspace)) {
        pane.applyDraftPlaceholderWorkspace({
          workspacePath: branch.worktreePath,
          worktreePath: branch.worktreePath,
          branch: branch.name,
        });
        addToast('info', `Selected existing worktree for ${branch.name}`);
      } else {
        addToast('info', 'Start the thread before checking out branches.');
      }
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
        if (workspaceChangingDisabled) {
          addToast('error', workspaceLock.reason);
          closeMenu();
          applying = false;
          return;
        }
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
    {#if intent.creatingBranch}
      <MenuItem
        label="Cancel new branch"
        onSelect={cancelCreate}
      >
        {#snippet icon()}
          <Icon icon={X} size={12} strokeWidth={2} />
        {/snippet}
      </MenuItem>
    {:else}
      <MenuItem
        label="New branch…"
        onSelect={startCreate}
      >
        {#snippet icon()}
          <Icon icon={Plus} size={12} strokeWidth={2} />
        {/snippet}
      </MenuItem>
    {/if}
    <MenuItem
      label={pruning ? 'Pruning…' : 'Prune stale branches'}
      disabled={pruning || !pane.threadId}
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
              label={truncateBranchLabel(branch.name)}
              suffix={branchBadge(branch)}
              checked={isBaseSelected(branch)}
              title={branchRowTitle(branch)}
              onSelect={() => selectBranch(branch)}
              action={showsSyncAction(branch) ? syncIcon : undefined}
              actionLabel={syncingBranch === branch.name
                ? `Syncing ${branch.name}`
                : `Sync ${branch.name} from upstream`}
              actionDisabled={!pane.threadId || !canSync(branch) || syncingBranch !== null}
              actionTitle={syncDisabledTitle(branch)}
              onAction={() => handleSync(branch)}
            />
          {/each}
        </div>
      {/if}
    {/if}
  </Menu>
</Popover>
