<script lang="ts">
  import RotateCcw from 'lucide-svelte/icons/rotate-ccw';
  import Icon from '../../primitives/Icon.svelte';
  import type { Checkpoint } from '../../../types/checkpoint';

  interface Props {
    visibleCheckpoints: Checkpoint[];
    selectedTurnCount: number | null;
    onSelectTurn: (turnCount: number | null) => void;
    showRevert: boolean;
    reverting: boolean;
    onRevertClick: () => void;
  }

  let {
    visibleCheckpoints,
    selectedTurnCount,
    onSelectTurn,
    showRevert,
    reverting,
    onRevertClick,
  }: Props = $props();
</script>

<div class="flex gap-1 overflow-x-auto border-t border-border-subtle px-3 py-2">
  <button
    class="shrink-0 rounded border px-2.5 py-1 text-[12px] {selectedTurnCount === null ? 'border-accent/60 bg-accent/15 text-accent' : 'border-border-subtle text-fg-muted hover:bg-surface-2'}"
    onclick={() => onSelectTurn(null)}
    data-testid="diff-all-turns"
  >
    All turns
  </button>
  {#each visibleCheckpoints as checkpoint (checkpoint.id)}
    <button
      class="shrink-0 rounded border px-2.5 py-1 text-[12px] {selectedTurnCount === checkpoint.checkpointTurnCount ? 'border-accent/60 bg-accent/15 text-accent' : 'border-border-subtle text-fg-muted hover:bg-surface-2'}"
      onclick={() => onSelectTurn(checkpoint.checkpointTurnCount)}
      data-testid={`diff-turn-${checkpoint.checkpointTurnCount}`}
    >
      {checkpoint.checkpointTurnCount === 0 ? 'Baseline' : `Turn ${checkpoint.checkpointTurnCount}`}
      {#if checkpoint.status && checkpoint.status !== 'ready'}
        <span class="ml-1 text-[10px] text-warning">{checkpoint.status}</span>
      {/if}
    </button>
  {/each}
  {#if showRevert}
    <button
      class="ml-auto inline-flex shrink-0 items-center gap-1 rounded border border-error/40 px-2.5 py-1 text-[12px] text-error hover:bg-error/10"
      onclick={onRevertClick}
      disabled={reverting}
      data-testid="diff-turn-revert"
    >
      <Icon icon={RotateCcw} size={13} />
      Revert
    </button>
  {/if}
</div>
