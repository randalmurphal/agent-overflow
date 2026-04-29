<script lang="ts">
  // Thin flex row rendered below the composer (by ChatView). Hosts the
  // env (workspace/worktree) picker on the left and the branch picker
  // on the right. Empty when no thread is active so it doesn't steal
  // vertical space during the "select a thread" view.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import EnvPicker from './EnvPicker.svelte';
  import BranchPicker from './BranchPicker.svelte';
  import WorktreeNameInput from './WorktreeNameInput.svelte';
  import { createWorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();
  let workspaceLock = createWorkspaceChangeLockState(() => pane);
</script>

{#if pane.thread}
  <div class="px-6">
    <div
      class="mx-auto flex w-full max-w-[68rem] items-center justify-between gap-2 py-1.5 text-[11px] text-text-secondary"
      data-testid="below-composer-bar"
    >
      <div class="flex min-w-0 items-center gap-2">
        <EnvPicker {pane} {workspaceLock} />
        <WorktreeNameInput {pane} />
      </div>
      <BranchPicker {pane} {workspaceLock} />
    </div>
  </div>
{/if}
