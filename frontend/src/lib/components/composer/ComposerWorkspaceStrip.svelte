<script lang="ts">
  // Workspace strip rendered INSIDE the composer card as the bottom-
  // most row. Project picker leads on the left (interactive while the
  // thread is still a draft, locked once it sends), then the env
  // (workspace/worktree) picker, then the worktree branch-name input
  // slotted next to it when the user has staged a new worktree, then
  // the branch picker. The whole group sits on the left so the strip
  // reads as a single "where am I" cluster rather than several opposing
  // controls.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import ProjectPicker from './workspace/ProjectPicker.svelte';
  import EnvPicker from './workspace/EnvPicker.svelte';
  import BranchPicker from './workspace/BranchPicker.svelte';
  import WorktreeNameInput from './workspace/WorktreeNameInput.svelte';
  import { createWorkspaceChangeLockState } from '../../stores/workspaceChangeLock.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();
  let workspaceLock = createWorkspaceChangeLockState(() => pane);
</script>

{#if pane.thread}
  <div
    class="flex min-w-0 items-center gap-2 border-t border-border-subtle px-3 py-1.5 text-[11px] text-fg-muted"
    data-testid="composer-workspace-strip"
  >
    <ProjectPicker {pane} />
    {#if pane.threadId}
      <EnvPicker {pane} {workspaceLock} />
      <BranchPicker {pane} {workspaceLock} />
      <WorktreeNameInput {pane} workspaceDirty={false} />
    {/if}
  </div>
{/if}
