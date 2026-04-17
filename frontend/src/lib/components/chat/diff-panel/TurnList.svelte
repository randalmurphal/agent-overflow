<script lang="ts">
  import type { Checkpoint } from '../../../types/checkpoint';
  import { relativeTime } from '../../../utils/format';

  interface Props {
    checkpoints: Checkpoint[];
    selectedTurnIndex: number | null;
    onSelect: (turnIndex: number) => void;
  }

  let { checkpoints, selectedTurnIndex, onSelect }: Props = $props();
</script>

<div class="flex flex-col divide-y divide-border overflow-y-auto" aria-label="Turn list">
  {#if checkpoints.length === 0}
    <div class="px-3 py-4 text-xs text-text-secondary">
      No turns checkpointed yet.
    </div>
  {:else}
    {#each checkpoints as cp (cp.id)}
      <button
        type="button"
        data-testid={`diff-turn-${cp.turnIndex}`}
        aria-current={selectedTurnIndex === cp.turnIndex}
        class={[
          'text-left px-3 py-2 cursor-pointer transition-colors',
          selectedTurnIndex === cp.turnIndex
            ? 'bg-accent/15 text-text-primary'
            : 'hover:bg-surface-2/40 text-text-secondary',
        ].join(' ')}
        onclick={() => onSelect(cp.turnIndex)}
      >
        <div class="flex items-center justify-between gap-2">
          <span class="font-mono text-xs">Turn {cp.turnIndex}</span>
          <span class="text-[10px] opacity-70">{relativeTime(cp.capturedAt)}</span>
        </div>
      </button>
    {/each}
  {/if}
</div>
