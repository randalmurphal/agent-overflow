<script lang="ts">
  import type { ThreadPane } from './thread.svelte';
  import { createWorkspaceChangeLockState, type WorkspaceChangeLockState } from './workspaceChangeLock.svelte';

  interface Props {
    pane: ThreadPane;
    expose?: (lock: WorkspaceChangeLockState) => void;
  }

  let { pane, expose }: Props = $props();
  let lock = createWorkspaceChangeLockState(() => pane);

  $effect(() => {
    expose?.(lock);
  });
</script>

<div
  data-testid="workspace-change-lock"
  data-locked={lock.locked ? 'true' : 'false'}
  data-reason={lock.reason}
  data-running-background-count={lock.runningBackgroundCount}
></div>
