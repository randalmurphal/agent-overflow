<script lang="ts">
  import DiffViewer from './DiffViewer.svelte';
  import Button from '../../primitives/Button.svelte';

  interface Props {
    loading: boolean;
    diffText: string;
    onRefresh: () => void;
  }

  let { loading, diffText, onRefresh }: Props = $props();
</script>

<div class="flex flex-1 min-h-0 flex-col" id="diff-panel-pane" role="tabpanel">
  <div class="flex items-center gap-2 border-b border-border px-3 py-2 text-xs">
    <span class="text-text-secondary">Uncommitted changes in the workspace.</span>
    <Button
      variant="secondary"
      size="xs"
      onclick={onRefresh}
      ariaLabel="Refresh working tree diff"
      testId="diff-worktree-refresh"
      class="ml-auto"
    >
      {#snippet children()}Refresh{/snippet}
    </Button>
  </div>
  <div class="flex-1 min-h-0 overflow-auto">
    {#if loading}
      <div class="px-3 py-6 text-sm text-text-secondary text-center" role="status" aria-live="polite">
        Loading working tree diff…
      </div>
    {:else}
      <DiffViewer diff={diffText} emptyMessage="Working tree is clean." />
    {/if}
  </div>
</div>
