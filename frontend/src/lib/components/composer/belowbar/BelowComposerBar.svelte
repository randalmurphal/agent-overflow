<script lang="ts">
  // Thin flex row rendered below the composer (by ChatView). Hosts the
  // env (workspace/worktree) picker on the left and the branch picker
  // on the right. Empty when no thread is active so it doesn't steal
  // vertical space during the "select a thread" view.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import EnvPicker from './EnvPicker.svelte';
  import BranchPicker from './BranchPicker.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();
</script>

{#if pane.thread}
  <div
    class="flex items-center justify-between gap-2 px-4 py-1.5 text-[11px] text-text-secondary"
    data-testid="below-composer-bar"
  >
    <EnvPicker {pane} />
    <BranchPicker {pane} />
  </div>
{/if}
