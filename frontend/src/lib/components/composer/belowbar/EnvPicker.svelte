<script lang="ts">
  // "Local ▾" trigger in the below-composer bar. Lists the thread's
  // project root plus any worktrees so the user can switch where the
  // provider operates without leaving the chat view.
  //
  // Selecting the project root or a worktree fires
  // UpdateThreadWorkspace; the backend restart flow handles reconnecting
  // the live session at the new path.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import { GitListWorktrees, UpdateThreadWorkspace } from '../../../stores/bindings';
  import { Worktree } from '../../../../../bindings/agent-overflow/internal/git/models';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let worktrees: Worktree[] = $state([]);
  let loading = $state(false);
  let applying = $state(false);

  let projectPath = $derived(pane.thread?.projectPath ?? '');
  let currentWorkspace = $derived(pane.thread?.workspacePath ?? '');
  let isAtProjectRoot = $derived(currentWorkspace === projectPath);

  // Short display label for the trigger. Prefer the worktree basename
  // so the bar doesn't swallow horizontal space; fall back to "Local"
  // when we're sitting at the project root.
  function basename(path: string): string {
    if (!path) return '';
    const trimmed = path.replace(/\/$/, '');
    const idx = trimmed.lastIndexOf('/');
    return idx >= 0 ? trimmed.slice(idx + 1) : trimmed;
  }

  let triggerLabel = $derived(isAtProjectRoot ? 'Local' : basename(currentWorkspace) || 'Local');

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
    const threadId = pane.thread.id;
    if (path === currentWorkspace) {
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
      addToast('error', `Failed to switch workspace: ${err}`);
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
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="Workspace" onClose={closeMenu}>
    <MenuItem
      label={projectPath ? `Local (${basename(projectPath)})` : 'Local'}
      checked={isAtProjectRoot}
      disabled={!projectPath}
      onSelect={() => selectPath(projectPath)}
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
        {#if wt.path !== projectPath}
          <MenuItem
            label={basename(wt.path) || wt.path}
            checked={currentWorkspace === wt.path}
            onSelect={() => selectPath(wt.path)}
          />
        {/if}
      {/each}
    {/if}
  </Menu>
</Popover>
