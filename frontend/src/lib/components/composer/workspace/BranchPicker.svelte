<script lang="ts">
  // Branch trigger + list for the below-composer bar. Two modes share the
  // same dropdown surface:
  //
  //   - Selection: pick a branch to checkout (idle thread) or to use as
  //     the base for staged new-worktree intent.
  //   - Creation: top "+ New branch…" row toggles an inline form
  //     [name] [From <base>] [Create] [Cancel]; the branch list below
  //     becomes a base picker — selecting a row sets the base instead
  //     of switching the workspace.
  //
  // When the workspace is dirty, both modes surface a "Local (with
  // changes)" entry at the top of the branch list. Picking it carries
  // the uncommitted changes; picking any real branch while dirty
  // performs a clean checkout (the destructive path is gated by an
  // explicit confirmation chip elsewhere in the create flow).

  import { tick } from 'svelte';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import GitBranchIcon from 'lucide-svelte/icons/git-branch';
  import Plus from 'lucide-svelte/icons/plus';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { GitBranch, GitStatus } from '../../../types/git';
  import type { Thread } from '../../../types/models';
  import {
    GetGitStatus,
    GetThread,
    GitCheckout,
    GitCreateBranchFrom,
    GitListBranches,
    UpdateThreadWorkspace,
  } from '../../../stores/bindings';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { sameNormalizedPath } from '../../../utils/path';
  import {
    LOCAL_BASE_SENTINEL,
    setWorktreeBaseBranch,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';
  import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';

  interface Props {
    pane: ThreadPane;
    workspaceLock: WorkspaceChangeLockState;
  }

  let { pane, workspaceLock }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let nameInputEl: HTMLInputElement | undefined = $state(undefined);
  let open = $state(false);
  let branches: GitBranch[] = $state([]);
  let query = $state('');
  let loading = $state(false);
  let applying = $state(false);
  let workspaceDirty = $state(false);

  // Inline branch-create form state. Lives alongside the picker so the
  // user can flip between "pick a base from the list" and the form
  // header without losing context.
  let creating = $state(false);
  let createName = $state('');
  let createBase = $state('');
  let createPending = $state(false);
  let createError: string | null = $state(null);

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
    if (intent.baseBranch === LOCAL_BASE_SENTINEL) return 'From Local (with changes)';
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
    if (creating) return createBase === LOCAL_BASE_SENTINEL;
    return intent.mode === 'new-worktree' && intent.baseBranch === LOCAL_BASE_SENTINEL;
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
    try {
      await Promise.all([fetchBranches, fetchStatus]);
    } finally {
      loading = false;
    }
  }

  function closeMenu(): void {
    open = false;
    query = '';
    cancelCreate();
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

  function handleCreateNameKeydown(event: KeyboardEvent): void {
    event.stopPropagation();
    if (event.key === 'Enter') {
      event.preventDefault();
      void performCreate();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      cancelCreate();
    }
  }

  async function startCreate(): Promise<void> {
    if (!pane.thread || workspaceChangingDisabled) return;
    creating = true;
    createName = '';
    // Default base mirrors the worktree intent flow: dirty workspace
    // pre-selects "Local (with changes)" so the destructive clean-checkout
    // path is opt-in. Otherwise, branch off the current HEAD.
    createBase = workspaceDirty ? LOCAL_BASE_SENTINEL : currentBranch;
    createError = null;
    await tick();
    nameInputEl?.focus();
  }

  function cancelCreate(): void {
    creating = false;
    createName = '';
    createBase = '';
    createError = null;
    createPending = false;
  }

  function pickCreateBase(name: string): void {
    createBase = name;
    createError = null;
  }

  // True when the user has picked a non-Local base while the workspace
  // is dirty. The backend treats this as "checkout the base, dropping
  // local changes" — surface a confirm chip rather than letting one
  // click silently destroy work.
  let createDiscardsChanges = $derived(
    creating && workspaceDirty && createBase !== LOCAL_BASE_SENTINEL && createBase !== '',
  );

  async function performCreate(): Promise<void> {
    if (!pane.thread || createPending) return;
    const name = createName.trim();
    if (!name) {
      createError = 'Branch name required';
      return;
    }
    if (!createBase) {
      createError = 'Pick a base';
      return;
    }
    createPending = true;
    createError = null;
    try {
      // The LOCAL sentinel maps to (current branch, carryLocalChanges=true)
      // on the wire. Anything else is a regular base; carry is false.
      const isLocal = createBase === LOCAL_BASE_SENTINEL;
      const baseForBackend = isLocal ? currentBranch : createBase;
      const updated = (await GitCreateBranchFrom(
        pane.thread.id,
        name,
        baseForBackend,
        isLocal,
      )) as Thread;
      syncThread(updated);
      addToast('info', `Created branch ${name}`);
      cancelCreate();
      closeMenu();
    } catch (err) {
      console.error('GitCreateBranchFrom failed:', err);
      createError = errString(err);
    } finally {
      createPending = false;
    }
  }

  function selectLocalRow(): void {
    if (!pane.thread) return;
    if (creating) {
      pickCreateBase(LOCAL_BASE_SENTINEL);
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
      pickCreateBase(branch.name);
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
    {#if creating}
      <div class="px-2 pb-1 pt-1 space-y-1.5" data-testid="branch-picker-create-form">
        <input
          bind:this={nameInputEl}
          type="text"
          value={createName}
          placeholder="New branch name"
          onkeydown={handleCreateNameKeydown}
          oninput={(e) => (createName = (e.target as HTMLInputElement).value)}
          class={[
            'h-7 w-72 rounded border border-border-subtle bg-surface-0',
            'px-2 text-xs text-text-primary placeholder:text-fg-hint',
            'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
          ].join(' ')}
        />
        <div class="flex items-center gap-2 text-[11px] text-fg-hint">
          <span>From:</span>
          <span class="truncate text-fg">
            {#if createBase === LOCAL_BASE_SENTINEL}
              Local (with changes)
            {:else if createBase}
              {createBase}
            {:else}
              <span class="text-fg-hint">Pick a base below</span>
            {/if}
          </span>
        </div>
        {#if createDiscardsChanges}
          <div class="text-[11px] text-warning" data-testid="branch-picker-create-discards">
            Discards uncommitted changes.
          </div>
        {/if}
        {#if createError}
          <div class="text-[11px] text-error truncate">{createError}</div>
        {/if}
        <div class="flex items-center justify-end gap-2">
          <button
            type="button"
            onclick={cancelCreate}
            disabled={createPending}
            class="rounded-[var(--radius-field)] px-2 py-0.5 text-[11px] text-fg-hint hover:bg-surface-2/70 hover:text-fg disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            type="button"
            onclick={performCreate}
            disabled={createPending || !createName.trim() || !createBase}
            data-testid="branch-picker-create-submit"
            class={[
              'rounded-[var(--radius-field)] px-2 py-0.5 text-[11px] disabled:opacity-50',
              createDiscardsChanges
                ? 'bg-error/15 text-error hover:bg-error/25'
                : 'bg-surface-2/70 text-fg hover:bg-surface-2',
            ].join(' ')}
          >
            {#if createPending}
              Creating…
            {:else if createDiscardsChanges}
              Discard and create
            {:else}
              Create
            {/if}
          </button>
        </div>
      </div>
      <MenuDivider />
    {:else}
      <MenuItem
        label="New branch…"
        disabled={workspaceChangingDisabled}
        title={workspaceChangingDisabled ? disabledReason : undefined}
        onSelect={() => void startCreate()}
      >
        {#snippet icon()}
          <Icon icon={Plus} size={12} strokeWidth={2} />
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
            />
          {/each}
        </div>
      {/if}
    {/if}
  </Menu>
</Popover>
