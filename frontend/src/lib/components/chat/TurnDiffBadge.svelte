<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { TurnDiffSummary } from '../../utils/turnDiffSummary';

  interface Props {
    pane: ThreadPane;
    turnIndex: number;
    summary: TurnDiffSummary;
  }

  let { pane, turnIndex, summary }: Props = $props();

  function open() {
    pane.diffPanel.selectCheckpointTurnCount(turnIndex + 1);
    pane.setDiffPanelOpen(true);
  }

  let fileLabel = $derived(`${summary.fileCount} file${summary.fileCount === 1 ? '' : 's'}`);
  let turnNumber = $derived(turnIndex + 1);
  let ariaLabel = $derived(
    `Open diff panel on turn ${turnNumber}: ${summary.insertions} insertions, ${summary.deletions} deletions across ${fileLabel}`,
  );
</script>

<div class="mb-2 flex justify-end" data-testid="turn-diff-badge-row">
  <button
    type="button"
    onclick={open}
    aria-label={ariaLabel}
    data-testid="turn-diff-badge"
    data-turn-index={turnIndex}
    title="Open Diff Panel on This Turn"
    class="inline-flex items-center gap-2 rounded-full border border-border bg-surface-1 px-3 py-1 text-xs text-text-secondary hover:bg-surface-2/60 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
  >
    <span aria-hidden="true" class="text-text-secondary/70">✎</span>
    {#if summary.insertions > 0}
      <span class="text-success tabular-nums">+{summary.insertions}</span>
    {/if}
    {#if summary.deletions > 0}
      <span class="text-error tabular-nums">−{summary.deletions}</span>
    {/if}
    <span aria-hidden="true" class="text-text-secondary/50">·</span>
    <span class="tabular-nums">{fileLabel}</span>
  </button>
</div>
