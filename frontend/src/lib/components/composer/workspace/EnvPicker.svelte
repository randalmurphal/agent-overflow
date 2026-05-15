<script lang="ts">
  // Workspace trigger in the below-composer bar. Lists the project root,
  // staged new-worktree intent, and registered worktrees so the user can
  // choose where the next provider turn runs without leaving the chat.
  //
  // Existing paths persist via UpdateThreadWorkspace. New worktree intent
  // is staged locally and materialized by the next send. Worktree cleanup
  // happens inline: a trash icon on each row morphs that row into a
  // confirmation strip — no separate modal surface.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Folder from 'lucide-svelte/icons/folder';
  import GitBranch from 'lucide-svelte/icons/git-branch';
  import Trash2 from 'lucide-svelte/icons/trash-2';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import {
    GitListWorktrees,
    GitWorktreeStatus,
    RemoveOtherWorktree,
    UpdateThreadWorkspace,
    WorktreeStatus,
  } from '../../../stores/bindings';
  import type { Worktree } from '../../../types/git';
  import { syncThread } from '../../../stores/panes.svelte';
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
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import Button from '../../primitives/Button.svelte';

  interface Props {
    pane: ThreadPane;
    workspaceLock: WorkspaceChangeLockState;
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
  let worktrees: Worktree[] = $state([]);
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
  let triggerLabel = $derived.by(() => {
    if (intent.mode === 'new-worktree') return 'New Worktree';
    if (isAtProjectRoot) return 'Local';
    return pathBasename(currentWorkspace) || 'Worktree';
  });
  let triggerIcon = $derived.by(() => {
    if (intent.mode === 'new-worktree') return GitBranch;
    return isAtProjectRoot ? Folder : GitBranch;
  });

  let disabledReason = $derived(workspaceLock.reason);
  let workspaceChangingDisabled = $derived(workspaceLock.locked);

  async function handleTrigger(): Promise<void> {
    open = !open;
    if (!open) {
      confirm = null;
      return;
    }
    await refreshWorktreeList();
  }

  async function refreshWorktreeList(): Promise<void> {
    if (!pane.thread) return;
    if (loading) return;
    loading = true;
    try {
      const res = (await GitListWorktrees(pane.thread.id)) as Worktree[] | null;
      worktrees = Array.isArray(res) ? res : [];
    } catch (err) {
      console.error('GitListWorktrees failed:', err);
      worktrees = [];
    } finally {
      loading = false;
    }
  }

  function closeMenu(): void {
    open = false;
    confirm = null;
    triggerEl?.focus();
  }

  async function selectPath(path: string): Promise<void> {
    if (!pane.thread || applying) return;
    if (workspaceChangingDisabled) return;
    const threadId = pane.thread.id;
    if (sameNormalizedPath(path, projectPath)) {
      setThreadEnvMode(pane.thread, 'local');
    } else {
      clearWorktreeIntent(threadId);
    }
    if (sameNormalizedPath(path, currentWorkspace)) {
      closeMenu();
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

  function selectNewWorktree(): void {
    if (!pane.thread) return;
    if (workspaceChangingDisabled) return;
    setThreadEnvMode(pane.thread, 'new-worktree');
    closeMenu();
  }

  async function requestRemove(wt: Worktree): Promise<void> {
    if (!pane.thread) return;
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
      const status = (await GitWorktreeStatus(pane.thread.id, wt.path)) as WorktreeStatus;
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

  async function performRemove(force: boolean): Promise<void> {
    if (!pane.thread || !confirm) return;
    const path = confirm.path;
    const label = confirm.label;
    confirm = { ...confirm, pending: true, error: null };
    try {
      await RemoveOtherWorktree(pane.thread.id, path, force);
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
      checked={isAtProjectRoot && intent.mode !== 'new-worktree'}
      disabled={!projectPath || workspaceChangingDisabled}
      title={workspaceChangingDisabled ? disabledReason : undefined}
      onSelect={() => selectPath(projectPath)}
    />
    <MenuItem
      label="New Worktree"
      checked={intent.mode === 'new-worktree'}
      disabled={workspaceChangingDisabled}
      title={workspaceChangingDisabled ? disabledReason : undefined}
      onSelect={selectNewWorktree}
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
                <div class="mt-1 text-[11px] text-fg-hint">Checking…</div>
              {:else if confirm.error}
                <div class="mt-1 text-[11px] text-error truncate">{confirm.error}</div>
              {:else if confirm.status}
                {#if isRiskyStatus(confirm.status)}
                  <div class="mt-1 text-[11px] text-warning truncate">
                    {riskSummary(confirm.status)}
                  </div>
                {:else if confirm.status.attachedThreads > 0}
                  <div class="mt-1 text-[11px] text-fg-hint truncate">
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
              checked={sameNormalizedPath(currentWorkspace, wt.path) && intent.mode !== 'new-worktree'}
              disabled={workspaceChangingDisabled}
              title={workspaceChangingDisabled ? disabledReason : undefined}
              onSelect={() => selectPath(wt.path)}
              actionLabel={`Remove worktree ${pathBasename(wt.path) || wt.path}`}
              actionPosition="end"
              onAction={() => requestRemove(wt)}
            >
              {#snippet action()}
                <Icon icon={Trash2} size={12} strokeWidth={2} />
              {/snippet}
            </MenuItem>
          {/if}
        {/if}
      {/each}
    {/if}
  </Menu>
</Popover>
