<script lang="ts">
  // Live interactive workspace strip for Composer. Separated from the readonly
  // strip so read-only hosts (AgentPane) never subscribe to workspaceChangeLock,
  // branch/worktree sync events, or composerPickerRegistry.
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MachinePicker from './workspace/MachinePicker.svelte';
  import EnvPicker from './workspace/EnvPicker.svelte';
  import BranchPicker from './workspace/BranchPicker.svelte';
  import WorktreeNameInput from './workspace/WorktreeNameInput.svelte';
  import UsageChip from './UsageChip.svelte';
  import { createWorkspaceChangeLockState } from '../../stores/workspaceChangeLock.svelte';
  import { hasMultipleBackends } from '../../stores/attachedBackends.svelte';

  interface Props {
    pane: ThreadPane;
    askCopy: boolean;
  }

  let { pane, askCopy }: Props = $props();
  let workspaceLock = createWorkspaceChangeLockState(() => pane);
</script>

<div
  class="flex min-w-0 items-center gap-2 border-t border-border-subtle px-3 py-1.5 text-[0.6875rem] text-fg-muted"
  data-testid="composer-workspace-strip"
>
  <div class="flex min-w-0 flex-1 items-center gap-2">
    {#if hasMultipleBackends() && !askCopy}
      <MachinePicker {pane} />
    {/if}
    {#if pane.workspace && !askCopy}
      <BranchPicker {pane} />
      <EnvPicker {pane} {workspaceLock} />
      <WorktreeNameInput {pane} {workspaceLock} />
    {/if}
  </div>
  <div class="ml-auto shrink-0 whitespace-nowrap">
    <UsageChip {pane} />
  </div>
</div>
