<script lang="ts">
  // Workspace trigger in the below-composer bar. Lists the project root,
  // staged new-worktree intent, and registered worktrees so the user can
  // choose where the next provider turn runs without leaving the chat.
  //
  // Existing paths persist via UpdateThreadWorkspace. New worktree intent
  // is staged locally and materialized by the next send.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import { GitListWorktrees, UpdateThreadWorkspace } from '../../../stores/bindings';
  import type { Worktree } from '../../../types/git';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { sameNormalizedPath } from '../../../utils/path';
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

  interface Props {
    pane: ThreadPane;
    workspaceLock: WorkspaceChangeLockState;
  }

  let { pane, workspaceLock }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let worktrees: Worktree[] = $state([]);
  let loading = $state(false);
  let applying = $state(false);

  let projectPath = $derived(pane.thread?.projectPath ?? '');
  let currentWorkspace = $derived(pane.thread?.workspacePath ?? '');
  let isAtProjectRoot = $derived(sameNormalizedPath(currentWorkspace, projectPath));
  let intent = $derived(worktreeIntentForThread(pane.thread));

  // Keep path details in menu descriptions. The trigger itself stays
  // mode-shaped so it matches t3-code's "Current checkout/worktree" labels.
  function basename(path: string): string {
    if (!path) return '';
    const trimmed = path.replace(/\/$/, '');
    const idx = trimmed.lastIndexOf('/');
    return idx >= 0 ? trimmed.slice(idx + 1) : trimmed;
  }

  let triggerLabel = $derived.by(() => {
    if (intent.mode === 'new-worktree') return 'New worktree';
    if (isAtProjectRoot) return 'Current checkout';
    return 'Current worktree';
  });
  let disabledReason = $derived(workspaceLock.reason);
  let workspaceChangingDisabled = $derived(workspaceLock.locked);

  async function handleTrigger(): Promise<void> {
    open = !open;
    if (!open) return;
    if (!pane.thread) return;
    if (worktrees.length > 0 || loading) return;
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
      pane.replaceThread(updated);
      replaceThread(updated);
      addToast('info', `Workspace switched to ${basename(path) || path}`);
    } catch (err) {
      console.error('UpdateThreadWorkspace failed:', err);
      addToast('error', `Failed to switch workspace: ${errString(err)}`);
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
</script>

<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread || applying}
  aria-haspopup="menu"
  aria-expanded={open}
  data-testid="env-picker-trigger"
  class={[
    'inline-flex items-center gap-1 rounded border border-border',
    'px-2 py-0.5 text-[11px] text-text-secondary',
    'transition-colors cursor-pointer',
    'hover:border-text-secondary hover:text-text-primary',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <span class="truncate max-w-[160px]">{triggerLabel}</span>
  <ChevronDown class="h-3 w-3" aria-hidden="true" />
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
      label="Current checkout"
      description={projectPath ? basename(projectPath) : undefined}
      checked={isAtProjectRoot && intent.mode !== 'new-worktree'}
      disabled={!projectPath || workspaceChangingDisabled}
      title={workspaceChangingDisabled ? disabledReason : undefined}
      onSelect={() => selectPath(projectPath)}
    />
    <MenuItem
      label="New worktree"
      description="Create before the next send"
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
          <MenuItem
            label="Current worktree"
            description={wt.branch ? `${wt.branch} · ${basename(wt.path) || wt.path}` : basename(wt.path) || wt.path}
            checked={sameNormalizedPath(currentWorkspace, wt.path) && intent.mode !== 'new-worktree'}
            disabled={workspaceChangingDisabled}
            title={workspaceChangingDisabled ? disabledReason : undefined}
            onSelect={() => selectPath(wt.path)}
          />
        {/if}
      {/each}
    {/if}
  </Menu>
</Popover>
