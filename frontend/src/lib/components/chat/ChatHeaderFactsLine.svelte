<script lang="ts">
  // The compact chat header's second row: `machine · project · branch ·
  // worktree`, the whole "where" of the thread on one full-width line.
  // Every segment is a direct tap target for its own picker (no roll-up
  // to guess at), and the pickers are the same menus the composer's
  // workspace strip opens on desktop, mounted here trigger-less and
  // anchored to their segment. The strip itself does not mount under
  // compact (ComposerWorkspaceStrip).
  //
  // Only the branch runs long, so it is the one segment that ellipsizes;
  // machine and project cap their width. The worktree is an icon pinned at
  // the end: a plain folder at the project root, the accent git folder in
  // a linked worktree. It is always there, so the way into the worktree
  // picker (where New worktree lives) never shrinks or scrolls away.
  // Staging a new worktree adds the branch-name row beneath the line.
  import Folder from '@lucide/svelte/icons/folder';
  import FolderGit2 from '@lucide/svelte/icons/folder-git-2';
  import GitBranchIcon from '@lucide/svelte/icons/git-branch';
  import Icon from '../primitives/Icon.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MachinePicker from '../composer/workspace/MachinePicker.svelte';
  import EnvPicker from '../composer/workspace/EnvPicker.svelte';
  import BranchPicker from '../composer/workspace/BranchPicker.svelte';
  import WorktreeNameInput from '../composer/workspace/WorktreeNameInput.svelte';
  import ChatHeaderProject from './ChatHeaderProject.svelte';
  import { headerSegmentClasses, headerSegmentSeparatorClasses } from './headerSegmentClasses';
  import { createWorkspaceChangeLockState } from '../../stores/workspaceChangeLock.svelte';
  import { hasMultipleBackends } from '../../stores/attachedBackends.svelte';
  import { worktreeIntentForThread } from '../../stores/worktreeIntent.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();
  const workspaceLock = createWorkspaceChangeLockState(() => pane);

  let machineEl: HTMLButtonElement | undefined = $state(undefined);
  let branchEl: HTMLButtonElement | undefined = $state(undefined);
  let worktreeEl: HTMLButtonElement | undefined = $state(undefined);
  let machinePicker: { openPicker(): void; label(): string; canPick(): boolean } | undefined = $state();
  let branchPicker: { openPicker(): void; label(): string } | undefined = $state();
  let envPicker: { openPicker(): void; label(): string; atBase(): boolean } | undefined = $state();

  // Machine mounts only with a second backend attached, the same rule as
  // the desktop strip: a single-computer app never names its computer.
  let showMachine = $derived(hasMultipleBackends());
  let machineLabel = $derived(machinePicker?.label() ?? 'Computer');
  let machinePickable = $derived(machinePicker?.canPick() ?? false);
  let hasProject = $derived(Boolean(pane.thread?.projectId));
  let branchLabel = $derived(branchPicker?.label() ?? pane.thread?.branch ?? 'No branch');
  let worktreeLabel = $derived(envPicker?.label() ?? 'Base');
  let atBase = $derived(envPicker?.atBase() ?? true);
  let intent = $derived(worktreeIntentForThread(pane.thread));
  let namingBranch = $derived(intent.creatingBranch || intent.mode === 'new-worktree');
</script>

{#if pane.thread}
  <div
    class="flex w-full min-w-0 items-center gap-1 pl-7"
    data-testid="chat-header-facts"
  >
    {#if showMachine}
      <button
        bind:this={machineEl}
        type="button"
        onclick={() => machinePicker?.openPicker()}
        disabled={!machinePickable}
        aria-haspopup={machinePickable ? 'menu' : undefined}
        title={`Computer: ${machineLabel}`}
        data-testid="chat-header-machine"
        data-locked={!machinePickable || undefined}
        class="{headerSegmentClasses} shrink-0 max-w-[7rem] text-text-secondary"
      >
        <span class="truncate">{machineLabel}</span>
      </button>
      <MachinePicker bind:this={machinePicker} {pane} hideTrigger anchor={machineEl} placement="bottom-start" />
      {#if hasProject}
        <span class={headerSegmentSeparatorClasses} aria-hidden="true">·</span>
      {/if}
    {/if}
    <ChatHeaderProject {pane} class="shrink-0 max-w-[9rem] text-text-secondary" />
    {#if showMachine || hasProject}
      <span class={headerSegmentSeparatorClasses} aria-hidden="true">·</span>
    {/if}
    <button
      bind:this={branchEl}
      type="button"
      onclick={() => branchPicker?.openPicker()}
      aria-haspopup="menu"
      title={`Branch: ${branchLabel}`}
      data-testid="chat-header-branch"
      class="{headerSegmentClasses} min-w-0 text-text-secondary"
    >
      <Icon icon={GitBranchIcon} size={11} strokeWidth={2} class="shrink-0 opacity-70" />
      <span class="truncate">{branchLabel}</span>
    </button>
    <BranchPicker bind:this={branchPicker} {pane} hideTrigger anchor={branchEl} />
    <span class={headerSegmentSeparatorClasses} aria-hidden="true">·</span>
    <button
      bind:this={worktreeEl}
      type="button"
      onclick={() => envPicker?.openPicker()}
      aria-haspopup="menu"
      aria-label={`Worktree: ${worktreeLabel}`}
      title={`Worktree: ${worktreeLabel}`}
      data-testid="chat-header-worktree"
      data-at-base={atBase || undefined}
      class="{headerSegmentClasses} shrink-0 {atBase ? 'text-text-secondary' : 'text-accent enabled:hover:text-accent'}"
    >
      <Icon icon={atBase ? Folder : FolderGit2} size={12} strokeWidth={2} class={atBase ? 'opacity-80' : ''} />
    </button>
    <EnvPicker bind:this={envPicker} {pane} {workspaceLock} hideTrigger anchor={worktreeEl} />
  </div>
  {#if namingBranch}
    <div class="flex w-full min-w-0 items-center pl-7" data-testid="chat-header-worktree-naming">
      <WorktreeNameInput {pane} workspaceDirty={false} {workspaceLock} />
    </div>
  {/if}
{/if}
