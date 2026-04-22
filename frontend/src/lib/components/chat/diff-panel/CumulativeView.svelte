<script lang="ts">
  import DiffViewer from './DiffViewer.svelte';
  import Button from '../../primitives/Button.svelte';
  import type { DiffStats } from '../../../utils/diffAggregation';

  interface Props {
    loading: boolean;
    diffText: string;
    stats: DiffStats;
    onRefresh: () => void;
  }

  let { loading, diffText, stats, onRefresh }: Props = $props();
</script>

<div class="flex flex-1 min-h-0 flex-col" id="diff-panel-pane" role="tabpanel">
  <div class="flex items-center gap-3 border-b border-border px-3 py-2 text-xs">
    <span class="text-text-secondary">
      {#if stats.fileCount === 0}
        No agent-authored diffs yet.
      {:else}
        {stats.fileCount} file{stats.fileCount === 1 ? '' : 's'}
      {/if}
    </span>
    {#if stats.insertions > 0 || stats.deletions > 0}
      <span class="flex gap-2 text-[11px] tabular-nums">
        {#if stats.insertions > 0}<span class="text-success">+{stats.insertions}</span>{/if}
        {#if stats.deletions > 0}<span class="text-error">-{stats.deletions}</span>{/if}
      </span>
    {/if}
    <Button
      variant="secondary"
      size="xs"
      onclick={onRefresh}
      ariaLabel="Refresh cumulative diff"
      testId="diff-cumulative-refresh"
      class="ml-auto"
    >
      {#snippet children()}Refresh{/snippet}
    </Button>
  </div>
  <div class="flex-1 min-h-0 overflow-auto">
    {#if loading}
      <div class="px-3 py-6 text-sm text-text-secondary text-center" role="status" aria-live="polite">
        Aggregating agent diffs…
      </div>
    {:else}
      <DiffViewer diff={diffText} emptyMessage="No agent-authored diffs in this thread yet." />
    {/if}
  </div>
</div>
