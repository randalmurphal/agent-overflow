<script lang="ts">
  import RotateCcw from 'lucide-svelte/icons/rotate-ccw';
  import Icon from '../../primitives/Icon.svelte';
  import type { Checkpoint } from '../../../types/checkpoint';

  interface Props {
    visibleCheckpoints: Checkpoint[];
    selectedUserItemId: string | null;
    onSelectCheckpoint: (userItemId: string | null) => void;
    showRevert: boolean;
    reverting: boolean;
    onRevertClick: () => void;
  }

  let {
    visibleCheckpoints,
    selectedUserItemId,
    onSelectCheckpoint,
    showRevert,
    reverting,
    onRevertClick,
  }: Props = $props();
</script>

<div class="flex gap-1 overflow-x-auto border-t border-border-subtle px-3 py-2">
  <button
    class="shrink-0 rounded border px-2.5 py-1 text-[12px] {selectedUserItemId === null ? 'border-accent/60 bg-accent/15 text-accent' : 'border-border-subtle text-fg-muted hover:bg-surface-2'}"
    onclick={() => onSelectCheckpoint(null)}
    data-testid="diff-all-messages"
  >
    All messages
  </button>
  {#each visibleCheckpoints as checkpoint (checkpoint.id)}
    <button
      class="shrink-0 rounded border px-2.5 py-1 text-[12px] {selectedUserItemId === checkpoint.userItemId ? 'border-accent/60 bg-accent/15 text-accent' : 'border-border-subtle text-fg-muted hover:bg-surface-2'}"
      onclick={() => onSelectCheckpoint(checkpoint.userItemId)}
      data-testid={`diff-message-${checkpoint.turnIndex}`}
    >
      {`Message ${checkpoint.turnIndex + 1}`}
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
      data-testid="diff-message-revert"
    >
      <Icon icon={RotateCcw} size={13} />
      Revert
    </button>
  {/if}
</div>
