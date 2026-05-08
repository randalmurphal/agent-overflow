<script lang="ts">
  // Workspace strip rendered INSIDE the composer card as the bottom-
  // most row. Branch picker leads on the left so the most-changed item
  // sits where the eye lands; env (workspace/worktree) picker trails on
  // the right with the worktree branch-name input slotted next to it
  // when the user has staged a new worktree.

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
    class="flex items-center justify-between gap-2 border-t border-border-subtle bg-surface-0 px-3 py-1.5 text-[11px] text-fg-muted"
    data-testid="composer-workspace-strip"
  >
    <BranchPicker {pane} {workspaceLock} />
    <div class="flex min-w-0 items-center gap-2">
      <EnvPicker {pane} {workspaceLock} />
      <WorktreeNameInput {pane} />
    </div>
  </div>
{/if}
