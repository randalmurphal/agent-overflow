<script lang="ts">
  // Workspace strip rendered INSIDE the composer card as the bottom-
  // most row. Env (workspace/worktree) picker leads on the left, then
  // the worktree branch-name input slotted next to it when the user
  // has staged a new worktree, then the branch picker. Both pickers
  // sit on the left so the strip reads as a single "where am I" group
  // rather than two opposing controls.

  import type { ThreadPane } from '../../stores/thread.svelte';
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
    <EnvPicker {pane} {workspaceLock} />
    <WorktreeNameInput {pane} />
    <BranchPicker {pane} {workspaceLock} />
  </div>
{/if}
