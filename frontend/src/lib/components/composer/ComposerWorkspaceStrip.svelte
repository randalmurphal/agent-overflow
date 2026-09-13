<script lang="ts">
  // Workspace strip rendered INSIDE the composer card as the bottom-
  // most row, desktop only. The machine picker leads (mounted only once
  // a second backend is attached), then the branch picker, the env
  // (workspace/worktree) picker with its optional worktree branch-name
  // input when the user has staged a new worktree, and the usage chip
  // pinned to the right as the opposing "what has this cost" element.
  // The project is not here: it is the header's crumb before the title
  // (chat/ChatHeaderProject.svelte), on every platform.
  //
  // Under compact the strip does not mount at all. The chat header's
  // facts line (chat/ChatHeaderFactsLine.svelte) shows and changes the
  // machine, project, branch and worktree there, and the usage chip
  // moves to the right end of the activity rail (ActivityRail.svelte).
  //
  // Two hosts, one strip: the main composer mounts it live, and the agent
  // pane's read-only shell mounts it with `readonly` — the same chips
  // with the same values (a subagent runs in the same thread, so env and
  // branch are literally this pane's facts), rendered inert (`inert`
  // kills pointer and focus in one attribute), with the usage slot
  // showing the SUBAGENT's own spend instead of the thread chip.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import MachinePicker from './workspace/MachinePicker.svelte';
  import EnvPicker from './workspace/EnvPicker.svelte';
  import BranchPicker from './workspace/BranchPicker.svelte';
  import WorktreeNameInput from './workspace/WorktreeNameInput.svelte';
  import UsageChip from './UsageChip.svelte';
  import { composerTriggerClasses } from './triggerClasses';
  import { createWorkspaceChangeLockState } from '../../stores/workspaceChangeLock.svelte';
  import { hasMultipleBackends } from '../../stores/attachedBackends.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';

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
</script>

{#if pane.thread && !isCompactLayout()}
  <div
    class="flex min-w-0 items-center gap-2 border-t border-border-subtle px-3 py-1.5 text-[0.6875rem] text-fg-muted"
    data-testid="composer-workspace-strip"
    inert={readonly || undefined}
  >
    <div class="flex min-w-0 flex-1 items-center gap-2">
      {#if hasMultipleBackends()}
        <!--
          Machine leads the cluster because it is the outermost "where":
          machine, then branch, then the checkout. Absent on a
          single-backend client (spec §10 ruling), so that app's strip is
          exactly the one below.
        -->
        <MachinePicker {pane} />
      {/if}
      <BranchPicker {pane} />
      <EnvPicker {pane} {workspaceLock} />
      {#if !readonly}
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
{/if}
