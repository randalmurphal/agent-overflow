<script lang="ts">
  // Workspace strip rendered INSIDE the composer card as the bottom-
  // most row. The project picker leads on the left (the machine picker
  // ahead of it once a second backend is attached). Threads additionally
  // surface the env (workspace/worktree) picker, an
  // optional worktree branch-name input when the user has staged a new
  // worktree, and the branch picker.
  // The whole group sits on the left so the strip reads as a single
  // "where am I" cluster rather than several opposing controls; the
  // usage chip is pinned to the right as the opposing "what has this
  // cost" element.
  //
  // Two hosts, one row: the main composer mounts it live, and the agent
  // pane's read-only shell mounts it with `readonly` — the same chips
  // with the same values (a subagent runs in the same thread, so mode,
  // project, env and branch are literally this pane's facts), rendered
  // inert (`inert` kills pointer and focus in one attribute), with the
  // usage slot showing the SUBAGENT's own spend instead of the thread
  // chip.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import MachinePicker from './workspace/MachinePicker.svelte';
  import ProjectPicker from './workspace/ProjectPicker.svelte';
  import EnvPicker from './workspace/EnvPicker.svelte';
  import BranchPicker from './workspace/BranchPicker.svelte';
  import WorktreeNameInput from './workspace/WorktreeNameInput.svelte';
  import UsageChip from './UsageChip.svelte';
  import { composerTriggerClasses } from './triggerClasses';
  import { createWorkspaceChangeLockState } from '../../stores/workspaceChangeLock.svelte';
  import { hasMultipleBackends } from '../../stores/attachedBackends.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { worktreeIntentForThread } from '../../stores/worktreeIntent.svelte';
  import Folder from '@lucide/svelte/icons/folder';
  import FolderGit2 from '@lucide/svelte/icons/folder-git-2';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';

  interface Props {
    pane: ThreadPane;
    /** Inert presentation for read-only hosts: no pointer, no focus. */
    readonly?: boolean;
    /**
     * Replaces the thread usage chip in the right slot (readonly hosts
     * show the scoped entity's own numbers, not the thread's). Empty
     * leaves the slot blank in readonly mode.
     */
    usageLabel?: string;
  }

  let { pane, readonly = false, usageLabel = '' }: Props = $props();
  let workspaceLock = createWorkspaceChangeLockState(() => pane);
  let workspaceTrigger: HTMLButtonElement | undefined = $state();
  let envPicker: { openPicker(): void; label(): string; atBase(): boolean } | undefined = $state();
  let branchPicker: { openPicker(): void; label(): string } | undefined = $state();
  let open = $state(false);
  let intent = $derived(worktreeIntentForThread(pane.thread));
  let branchLabel = $derived(branchPicker?.label() ?? pane.thread?.branch ?? 'Workspace');
  let workspaceLabel = $derived(envPicker?.label() ?? 'Workspace');
  let atBase = $derived(envPicker?.atBase() ?? true);
  let creatingBranch = $derived(intent.creatingBranch);
  let newWorktree = $derived(intent.mode === 'new-worktree');
  let namingBranch = $derived(creatingBranch || newWorktree);

  // A branch/worktree choice can enter the naming flow from its own picker.
  // Open the compact sheet for that transition; never widen the footer.
  $effect(() => {
    if (isCompactLayout() && (creatingBranch || newWorktree) && !readonly) open = true;
  });

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl: workspaceTrigger });
  }
  function choose(picker: { openPicker(): void } | undefined): void {
    open = false;
    picker?.openPicker();
  }
</script>

{#if pane.thread}
  <div
    class="flex min-w-0 items-center gap-2 border-t border-border-subtle px-3 py-1.5 text-[0.6875rem] text-fg-muted"
    data-testid="composer-workspace-strip"
    inert={readonly || undefined}
  >
    <div class="flex min-w-0 flex-1 items-center gap-2 compact:gap-1">
    {#if hasMultipleBackends()}
      <!--
        Machine leads the cluster because it is the outermost "where":
        machine, then project, then worktree, then branch. Absent on a
        single-backend client (spec §10 ruling), so that app's strip is
        exactly the one below.
      -->
      <MachinePicker {pane} />
    {/if}
    <ProjectPicker {pane} />
    {#if isCompactLayout()}
      <button
        bind:this={workspaceTrigger}
        type="button"
        class="{composerTriggerClasses} max-w-[12rem]"
        aria-label={`Workspace: ${workspaceLabel}, branch: ${branchLabel}`}
        title={`${workspaceLabel} · ${branchLabel}`}
        aria-haspopup="menu"
        aria-expanded={open}
        data-testid="workspace-picker-trigger"
        onclick={() => (open = !open)}
      >
        <Icon icon={atBase ? Folder : FolderGit2} size={12} class="shrink-0 opacity-70" />
        <span class="min-w-0 truncate text-fg">{branchLabel}</span>
        <Icon icon={ChevronDown} size={12} class="shrink-0 opacity-60" />
      </button>
    {/if}
    <EnvPicker bind:this={envPicker} {pane} {workspaceLock} hideTrigger={isCompactLayout()} anchor={workspaceTrigger} />
    <BranchPicker bind:this={branchPicker} {pane} hideTrigger={isCompactLayout()} anchor={workspaceTrigger} />
    {#if !readonly && !isCompactLayout()}
      <WorktreeNameInput {pane} workspaceDirty={false} {workspaceLock} />
    {/if}
    </div>
    <div class="ml-auto shrink-0 whitespace-nowrap">
      {#if readonly}
        {#if usageLabel}
          <span class="{composerTriggerClasses} tabular-nums" data-testid="workspace-strip-usage">
            {usageLabel}
          </span>
        {/if}
      {:else}
        <UsageChip {pane} />
      {/if}
    </div>
  </div>
  <Popover anchor={workspaceTrigger} open={open && isCompactLayout()} onClose={close} placement="top-start" role="none">
    <Menu ariaLabel="Workspace options" onClose={close}>
      <MenuItem label="Worktree" suffix={workspaceLabel} onSelect={() => choose(envPicker)} />
      <MenuItem label="Branch" suffix={branchLabel} onSelect={() => choose(branchPicker)} />
      {#if !readonly && namingBranch}
        <div class="border-t border-border-subtle px-3 py-2">
          <WorktreeNameInput {pane} workspaceDirty={false} {workspaceLock} />
        </div>
      {/if}
    </Menu>
  </Popover>
{/if}
