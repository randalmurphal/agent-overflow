<script lang="ts">
  import type { Checkpoint } from '../../../types/checkpoint';
  import type { DiffPanelState, TurnCompareMode } from '../../../stores/diffPanel.svelte';
  import TurnList from './TurnList.svelte';
  import DiffViewer from './DiffViewer.svelte';

  interface Props {
    store: DiffPanelState;
    checkpoints: Checkpoint[];
    loadingDiff: boolean;
    diffText: string;
    onSelectTurn: (turnIndex: number) => void;
    onCompareModeChange: (mode: TurnCompareMode) => void;
    /** Open the revert dialog for the given turn. Wired by DiffPanelDrawer. */
    onRequestRevert: (turnIndex: number) => void;
  }

  let { store, checkpoints, loadingDiff, diffText, onSelectTurn, onCompareModeChange, onRequestRevert }: Props = $props();

  let selected = $derived(store.selectedTurnIndex);
  let compareMode = $derived(store.turnCompareMode);
</script>

<div class="flex flex-1 min-h-0" id="diff-panel-pane" role="tabpanel">
  <aside class="w-48 shrink-0 border-r border-border bg-surface-1/50 flex flex-col min-h-0">
    <TurnList {checkpoints} selectedTurnIndex={selected} onSelect={onSelectTurn} />
  </aside>
  <div class="flex flex-col flex-1 min-h-0">
    <div class="flex items-center gap-2 border-b border-border px-3 py-2 text-xs">
      <span class="text-text-secondary">Compare:</span>
      <div
        role="radiogroup"
        aria-label="Turn compare mode"
        class="flex gap-1 rounded border border-border bg-surface-1 p-0.5"
      >
        <button
          type="button"
          role="radio"
          aria-checked={compareMode === 'next'}
          data-testid="diff-turn-compare-next"
          class={[
            'px-2 py-0.5 rounded text-xs cursor-pointer transition-colors',
            compareMode === 'next'
              ? 'bg-accent/25 text-accent'
              : 'text-text-secondary hover:bg-surface-2/50',
          ].join(' ')}
          onclick={() => onCompareModeChange('next')}
        >
          Turn → Next turn
        </button>
        <button
          type="button"
          role="radio"
          aria-checked={compareMode === 'worktree'}
          data-testid="diff-turn-compare-worktree"
          class={[
            'px-2 py-0.5 rounded text-xs cursor-pointer transition-colors',
            compareMode === 'worktree'
              ? 'bg-accent/25 text-accent'
              : 'text-text-secondary hover:bg-surface-2/50',
          ].join(' ')}
          onclick={() => onCompareModeChange('worktree')}
        >
          Turn → Worktree
        </button>
      </div>
      {#if selected !== null}
        <button
          type="button"
          data-testid="diff-turn-revert"
          onclick={() => onRequestRevert(selected!)}
          class="ml-auto px-2 py-0.5 rounded text-xs text-error border border-error/40 hover:bg-error/10 cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Revert to turn {selected}…
        </button>
      {/if}
    </div>
    <div class="flex-1 min-h-0 overflow-auto">
      {#if selected === null}
        <div class="px-3 py-6 text-sm text-text-secondary text-center">
          Select a turn on the left to view its diff.
        </div>
      {:else if loadingDiff}
        <div class="px-3 py-6 text-sm text-text-secondary text-center" role="status" aria-live="polite">
          Loading turn {selected}…
        </div>
      {:else}
        <DiffViewer diff={diffText} emptyMessage="No changes in this turn." />
      {/if}
    </div>
  </div>
</div>
