<script lang="ts">
  // Workspace strip rendered INSIDE the composer card as the bottom-
  // most row. Thread-mode picker leads on the left (interactive while
  // the thread is a draft, read-only label once committed since mode
  // is post-creation-immutable), then the project picker. Chat threads
  // additionally surface the env (workspace/worktree) picker, an
  // optional worktree branch-name input when the user has staged a new
  // worktree, and the branch picker. Design threads stop after the
  // project — they operate against the project root and have no
  // worktree/branch surface to switch.
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
  import ThreadModePicker from './workspace/ThreadModePicker.svelte';
  import ProjectPicker from './workspace/ProjectPicker.svelte';
  import EnvPicker from './workspace/EnvPicker.svelte';
  import BranchPicker from './workspace/BranchPicker.svelte';
  import WorktreeNameInput from './workspace/WorktreeNameInput.svelte';
  import UsageChip from './UsageChip.svelte';
  import { composerTriggerClasses } from './triggerClasses';
  import { createWorkspaceChangeLockState } from '../../stores/workspaceChangeLock.svelte';

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
  let isDesignThread = $derived(pane.thread?.mode === 'design');
</script>

{#if pane.thread}
  <div
    class="flex min-w-0 items-center gap-2 border-t border-border-subtle px-3 py-1.5 text-[0.6875rem] text-fg-muted"
    data-testid="composer-workspace-strip"
    inert={readonly || undefined}
  >
    <ThreadModePicker {pane} />
    <ProjectPicker {pane} />
    {#if !isDesignThread}
      <EnvPicker {pane} {workspaceLock} />
      <BranchPicker {pane} />
      {#if !readonly}
        <WorktreeNameInput {pane} workspaceDirty={false} {workspaceLock} />
      {/if}
    {/if}
    <div class="ml-auto">
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
