<script lang="ts">
  // Workspace trigger in the below-composer bar. Lists the project root,
  // staged new-worktree intent, and registered worktrees so the user can
  // choose where the next provider turn runs without leaving the chat.
  //
  // Choosing a worktree is durable draft state. A placeholder materializes
  // as soon as the user selects New Worktree or an existing worktree, so the
  // checkout has a thread owner and every workspace-keyed surface can follow
  // it before the first message. Worktree cleanup happens inline: a trash
  // icon on each row morphs that row into a confirmation strip.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Folder from '@lucide/svelte/icons/folder';
  import FolderGit2 from '@lucide/svelte/icons/folder-git-2';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import {
    GitListWorktrees,
    GitListWorktreesForProject,
    GitWorktreeStatus,
    GitWorktreeStatusForProject,
    RemoveOtherWorktreeForProject,
    RemoveOtherWorktree,
    UpdateThreadWorkspace,
    type GitWorkspaceState,
    WorktreeStatus,
    type WorktreeListItem,
  } from '../../../stores/bindings';
  import { forEachDraftPlaceholderPane, syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { userFacingError } from '../../../utils/userFacingError';
  import { sameNormalizedPath } from '../../../utils/path';
  import { pathBasename } from '../../../utils/pathDisplay';
  import {
    clearWorktreeIntent,
    setThreadEnvMode,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';
  import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';
  import { wailsEventOn } from '../../../stores/wailsEvents';
  import { debounce } from '../../../utils/debounce';
  import Popover from '../../primitives/Popover.svelte';
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import Button from '../../primitives/Button.svelte';

  interface Props {
    pane: ThreadPane;
    workspaceLock: WorkspaceChangeLockState;
  }

  /** The shape `applyDraftPlaceholderWorkspace` takes, named so the
   *  worktree-removal fan-out can pass one value to several panes. */
  interface PlaceholderWorkspace {
    workspacePath: string;
    worktreePath: string;
    branch: string;
  }

  interface ConfirmState {
    path: string;
    label: string;
    branch: string;
    status: WorktreeStatus | null;
    loading: boolean;
    pending: boolean;
    error: string | null;
  }

  let { pane, workspaceLock }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let worktrees: WorktreeListItem[] = $state([]);
  let loading = $state(false);
  let applying = $state(false);
  let confirm: ConfirmState | null = $state(null);

  let projectPath = $derived(pane.thread?.projectPath ?? '');
  let currentWorkspace = $derived(pane.thread?.workspacePath ?? '');
  let isAtProjectRoot = $derived(sameNormalizedPath(currentWorkspace, projectPath));
  let intent = $derived(worktreeIntentForThread(pane.thread));

  // Trigger reflects *where you are*, not the picker's mode. "Local"
  // sits at the project root; the worktree's basename otherwise. Staged
  // new-worktree intent overrides both so the user sees that the next
  // send will run somewhere new.
  let stagingNewWorktree = $derived(intent.mode === 'new-worktree');
  let triggerLabel = $derived.by(() => {
    if (stagingNewWorktree) return 'New Worktree';
    if (isAtProjectRoot) return 'Local';
    return pathBasename(currentWorkspace) || 'Worktree';
  });
  let triggerIcon = $derived.by(() => {
    if (stagingNewWorktree) return FolderGit2;
    return isAtProjectRoot ? Folder : FolderGit2;
  });
  let triggerIconName = $derived.by(() => {
    if (stagingNewWorktree) return 'folder-git-2';
    return isAtProjectRoot ? 'folder' : 'folder-git-2';
  });

  // Every row here MOVES this thread; none mutates the directory it leaves.
  // So the gate is the thread view of the lock, not the directory view: a
  // sibling thread responding at the project root must not pin this one
  // there. The per-row trash is directory-destructive and is gated by the
  // backend's own per-row `deleteBlocked` instead.
  let disabledReason = $derived(workspaceLock.threadReason);
  let workspaceChangingDisabled = $derived(workspaceLock.threadLocked);

  async function handleTrigger(): Promise<void> {
    open = !open;
    if (!open) {
      confirm = null;
      return;
    }
    // The placeholder only shows for the open's first fetch; the live
    // refreshes below swap the list in place.
    loading = true;
    await refreshWorktreeList();
  }

  // Sequence token instead of an in-flight guard: an event-triggered
  // refresh arriving during a fetch must still run (dropping it would
  // leave just-started activity unreflected); stale responses lose to
  // the latest call instead.
  let fetchSeq = 0;

  async function refreshWorktreeList(): Promise<void> {
    if (!pane.thread) return;
    const projectId = pane.thread.projectId;
    if (!pane.threadId && !projectId) return;
    const seq = ++fetchSeq;
    try {
      let res: WorktreeListItem[] | null;
      if (pane.threadId) {
        res = (await GitListWorktrees(pane.threadId)) as WorktreeListItem[] | null;
      } else {
        res = (await GitListWorktreesForProject(projectId!)) as WorktreeListItem[] | null;
      }
      if (seq !== fetchSeq) return;
      worktrees = Array.isArray(res) ? res : [];
    } catch (err) {
      console.error('GitListWorktrees failed:', err);
      if (seq !== fetchSeq) return;
      worktrees = [];
    } finally {
      if (seq === fetchSeq) loading = false;
    }
  }

  // deleteBlocked is a point-in-time flag: a turn starting or ending
  // anywhere in the project flips it, and this popover can sit open
  // across that. Re-fetch on the live activity signals so the rows never
  // go stale in either direction. Trailing debounce because turn events
  // fire per wire round — several per second while a pane streams.
  $effect(() => {
    if (!open) return;
    const scheduleRefresh = debounce(() => {
      void refreshWorktreeList();
    }, 250);
    const cancels = [
      wailsEventOn('provider:turn_started', scheduleRefresh),
      wailsEventOn('provider:turn_completed', scheduleRefresh),
      wailsEventOn('provider:background_tasks_changed', scheduleRefresh),
      wailsEventOn('provider:background_task_state', scheduleRefresh),
    ];
    return () => {
      scheduleRefresh.cancel();
      for (const cancel of cancels) cancel();
    };
  });

  function closeMenu(reason?: PopoverCloseReason): void {
    open = false;
    confirm = null;
    restorePickerFocus(reason, { triggerEl });
  }

  async function selectPath(path: string): Promise<void> {
    if (!pane.thread || applying) return;
    if (workspaceChangingDisabled) return;
    const threadId = pane.thread.id;
    const selectingProjectRoot = sameNormalizedPath(path, projectPath);
    if (selectingProjectRoot || pane.hasDraftPlaceholder) {
      setThreadEnvMode(pane.thread, 'local');
    } else {
      clearWorktreeIntent(threadId);
    }
    if (
      sameNormalizedPath(path, currentWorkspace) &&
      (!pane.hasDraftPlaceholder || selectingProjectRoot)
    ) {
      closeMenu();
      return;
    }
    if (pane.hasDraftPlaceholder) {
      const worktree = worktrees.find((candidate) => sameNormalizedPath(candidate.path, path));
      const placeholderId = pane.draftPlaceholder?.id ?? '';
      const previous = {
        workspacePath: pane.thread.workspacePath ?? projectPath,
        worktreePath: pane.thread.worktreePath ?? '',
        branch: pane.thread.branch ?? '',
      };
      pane.applyDraftPlaceholderWorkspace({
        workspacePath: path,
        worktreePath: sameNormalizedPath(path, projectPath) ? '' : path,
        branch: worktree?.branch,
      });
      applying = true;
      try {
        const createdId = await pane.ensureMaterializedThread();
        if (!createdId && pane.draftPlaceholder?.id === placeholderId) {
          pane.applyDraftPlaceholderWorkspace(previous);
        } else if (createdId) {
          clearWorktreeIntent(createdId);
        }
      } finally {
        applying = false;
        closeMenu();
      }
      return;
    }
    applying = true;
    try {
      const updated = (await UpdateThreadWorkspace(threadId, path)) as Thread;
      syncThread(updated);
      addToast('info', `Workspace switched to ${pathBasename(path) || path}`);
    } catch (err) {
      console.error('UpdateThreadWorkspace failed:', err);
      addToast('error', userFacingError(err));
    } finally {
      applying = false;
      closeMenu();
    }
  }

  async function selectNewWorktree(): Promise<void> {
    if (!pane.thread) return;
    if (workspaceChangingDisabled) return;
    setThreadEnvMode(pane.thread, 'new-worktree');
    if (!pane.hasDraftPlaceholder) {
      closeMenu();
      return;
    }
    applying = true;
    try {
      await pane.ensureMaterializedThread();
    } finally {
      applying = false;
      closeMenu();
    }
  }

  async function requestRemove(wt: WorktreeListItem): Promise<void> {
    if (!pane.thread) return;
    const projectId = pane.thread.projectId;
    if (!pane.threadId && !projectId) return;
    confirm = {
      path: wt.path,
      label: pathBasename(wt.path) || wt.path,
      branch: wt.branch ?? '',
      status: null,
      loading: true,
      pending: false,
      error: null,
    };
    try {
      const status = pane.threadId
        ? (await GitWorktreeStatus(pane.threadId, wt.path)) as WorktreeStatus
        : (await GitWorktreeStatusForProject(projectId!, wt.path)) as WorktreeStatus;
      // Guard against the user clicking Cancel and then opening a
      // different row's confirmation between the request and the
      // response — only apply the result if the active confirm is
      // still for this path.
      if (confirm && confirm.path === wt.path) {
        confirm = { ...confirm, status, loading: false };
      }
    } catch (err) {
      console.error('GitWorktreeStatus failed:', err);
      if (confirm && confirm.path === wt.path) {
        confirm = { ...confirm, loading: false, error: userFacingError(err) };
      }
    }
  }

  function cancelRemove(): void {
    confirm = null;
  }

  // Mirrors the backend's RemoveOtherWorktree refusal gate. "No upstream"
  // alone isn't a refusal — a freshly-created worktree off main rarely
  // has one, and treating it as risky would force the destructive button
  // path on a clean removal. Surface no-upstream as informational text in
  // riskSummary instead.
  function isRiskyStatus(s: WorktreeStatus | null): boolean {
    if (!s) return false;
    return s.dirty || s.unpushedCommits > 0;
  }

  function riskSummary(s: WorktreeStatus | null): string {
    if (!s) return '';
    const parts: string[] = [];
    if (s.dirty) {
      parts.push(`${s.uncommittedCount} uncommitted file${s.uncommittedCount === 1 ? '' : 's'}`);
    }
    if (s.unpushedCommits > 0) {
      parts.push(`${s.unpushedCommits} unpushed commit${s.unpushedCommits === 1 ? '' : 's'}`);
    }
    if (!s.hasUpstream && s.branch) {
      parts.push('no upstream');
    }
    if (s.attachedThreads > 0) {
      parts.push(`${s.attachedThreads} thread${s.attachedThreads === 1 ? '' : 's'} attached`);
    }
    return parts.join(' · ');
  }

  // A removed worktree's directory is gone, so every open draft composer
  // parked in it has to move — and a draft has no thread row for the
  // backend's attached-thread reattachment to reach. They go to the project
  // root, which is where the backend puts attached threads too.
  function moveDraftPlaceholdersOffWorktree(
    projectId: string,
    removedPath: string,
    rootState: PlaceholderWorkspace | null,
  ): void {
    forEachDraftPlaceholderPane(projectId, (target) => {
      if (!sameNormalizedPath(target.thread?.workspacePath ?? '', removedPath)) return;
      const root = target.thread?.projectPath ?? '';
      if (!root) return;
      target.applyDraftPlaceholderWorkspace(
        rootState && sameNormalizedPath(rootState.workspacePath, root)
          ? rootState
          // Nothing told us the root's branch, and the removed worktree's is
          // certainly wrong; '' renders as "No branch" until the next read.
          : { workspacePath: root, worktreePath: '', branch: '' },
      );
    });
  }

  async function performRemove(force: boolean): Promise<void> {
    if (!pane.thread || !confirm) return;
    const projectId = pane.thread.projectId ?? '';
    if (!pane.threadId && !projectId) return;
    const path = confirm.path;
    const label = confirm.label;
    const placeholderId = pane.draftPlaceholder?.id ?? '';
    const requestedWorkspace = pane.thread.workspacePath ?? '';
    confirm = { ...confirm, pending: true, error: null };
    try {
      let rootState: PlaceholderWorkspace | null = null;
      if (pane.threadId) {
        await RemoveOtherWorktree(pane.threadId, path, force);
      } else {
        const next = (await RemoveOtherWorktreeForProject(
          projectId,
          requestedWorkspace,
          path,
          force,
        )) as GitWorkspaceState;
        rootState = {
          workspacePath: next.workspacePath,
          worktreePath: next.worktreePath ?? '',
          branch: next.branch,
        };
        // Guarded, not aborted: the removal happened whatever this pane did
        // under the await, and the other panes still have to be told.
        if (
          pane.draftPlaceholder?.id === placeholderId &&
          sameNormalizedPath(pane.thread?.workspacePath ?? '', requestedWorkspace)
        ) {
          pane.applyDraftPlaceholderWorkspace(rootState);
        }
      }
      moveDraftPlaceholdersOffWorktree(projectId, path, rootState);
      addToast('info', `Removed worktree ${label}`);
      // If we just removed the current workspace, the backend has flipped
      // us to the project root and broadcast a thread upsert; the pane
      // store handles that sync. Either way, refresh the list so the row
      // disappears.
      confirm = null;
      await refreshWorktreeList();
    } catch (err) {
      console.error('RemoveOtherWorktree failed:', err);
      if (confirm) {
        confirm = { ...confirm, pending: false, error: userFacingError(err) };
      }
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
  data-trigger-icon={triggerIconName}
  data-testid="env-picker-trigger"
  class={composerTriggerClasses}
>
  <Icon icon={triggerIcon} size={12} strokeWidth={2} class="opacity-70" />
  <span class="truncate max-w-[160px] text-fg">{triggerLabel}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>

<Popover
  anchor={triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="Workspace" onClose={closeMenu}>
    <MenuItem
      label={projectPath ? `Local · ${pathBasename(projectPath)}` : 'Local'}
      checked={isAtProjectRoot && !stagingNewWorktree}
      disabled={!projectPath || workspaceChangingDisabled}
      title={workspaceChangingDisabled ? disabledReason : undefined}
      onSelect={() => selectPath(projectPath)}
    />
    <MenuItem
      label="New Worktree"
      checked={stagingNewWorktree}
      disabled={workspaceChangingDisabled}
      title={workspaceChangingDisabled ? disabledReason : undefined}
      onSelect={() => selectNewWorktree()}
    />
    {#if loading}
      <div
        class="px-3 py-1.5 text-xs text-text-secondary/60"
        role="presentation"
        data-testid="env-picker-loading"
      >
        Loading worktrees…
      </div>
    {:else if worktrees.length > 0}
      <MenuDivider />
      {#each worktrees as wt (wt.path)}
        {#if !sameNormalizedPath(wt.path, projectPath)}
          {#if confirm && confirm.path === wt.path}
            <div
              class="px-3 py-2 text-sm"
              role="presentation"
              data-testid="env-picker-confirm-row"
            >
              <div class="text-fg truncate">
                Remove <span class="font-medium">{confirm.label}</span>
                {#if confirm.branch}
                  <span class="text-fg-hint">· {confirm.branch}</span>
                {/if}?
              </div>
              {#if confirm.loading}
                <div class="mt-1 text-[0.6875rem] text-fg-hint">Checking…</div>
              {:else if confirm.error}
                <div class="mt-1 text-[0.6875rem] text-error truncate">{confirm.error}</div>
              {:else if confirm.status}
                {#if isRiskyStatus(confirm.status)}
                  <div class="mt-1 text-[0.6875rem] text-warning truncate">
                    {riskSummary(confirm.status)}
                  </div>
                {:else if confirm.status.attachedThreads > 0}
                  <div class="mt-1 text-[0.6875rem] text-fg-hint truncate">
                    {riskSummary(confirm.status)} — will move to project root.
                  </div>
                {/if}
              {/if}
              <div class="mt-2 flex items-center justify-end gap-2">
                <Button
                  variant="ghost"
                  size="xs"
                  onclick={cancelRemove}
                  disabled={confirm.pending}
                >
                  Cancel
                </Button>
                {#if confirm.status && isRiskyStatus(confirm.status)}
                  <Button
                    variant="danger"
                    size="xs"
                    onclick={() => performRemove(true)}
                    disabled={confirm.pending || confirm.loading}
                    testId="env-picker-confirm-force"
                  >
                    {confirm.pending ? 'Removing…' : 'Discard and remove'}
                  </Button>
                {:else}
                  <Button
                    variant="secondary"
                    size="xs"
                    onclick={() => performRemove(false)}
                    disabled={confirm.pending || confirm.loading}
                    testId="env-picker-confirm-remove"
                  >
                    {confirm.pending ? 'Removing…' : 'Remove'}
                  </Button>
                {/if}
              </div>
            </div>
          {:else}
            <MenuItem
              label={wt.branch ? `${pathBasename(wt.path) || wt.path} · ${wt.branch}` : pathBasename(wt.path) || wt.path}
              checked={sameNormalizedPath(currentWorkspace, wt.path) && !stagingNewWorktree}
              disabled={workspaceChangingDisabled}
              title={workspaceChangingDisabled ? disabledReason : undefined}
              onSelect={() => selectPath(wt.path)}
              actionLabel={`Remove worktree ${pathBasename(wt.path) || wt.path}`}
              actionPosition="end"
              actionDisabled={(!pane.threadId && !pane.thread?.projectId) || wt.deleteBlocked}
              actionTitle={wt.deleteBlocked
                ? 'This worktree cannot be removed while an attached thread is running.'
                : `Remove worktree ${pathBasename(wt.path) || wt.path}`}
              onAction={() => requestRemove(wt)}
            >
              {#snippet action()}
                {#if wt.deleteBlocked}
                  <!-- Same pulsing running-dot as the sidebar: a faded
                       trash icon reads as deletable at a glance; a live
                       activity marker doesn't. -->
                  <span
                    class="h-1.5 w-1.5 rounded-full bg-success animate-pulse"
                    data-testid={`env-picker-busy-${pathBasename(wt.path) || wt.path}`}
                  ></span>
                {:else}
                  <Icon icon={Trash2} size={12} strokeWidth={2} />
                {/if}
              {/snippet}
            </MenuItem>
          {/if}
        {/if}
      {/each}
    {/if}
  </Menu>
</Popover>
