<script lang="ts">
  import LocateFixed from 'lucide-svelte/icons/locate-fixed';
  import Icon from '../../primitives/Icon.svelte';
  import type { Checkpoint } from '../../../types/checkpoint';

  interface Props {
    visibleCheckpoints: Checkpoint[];
    selectedUserItemId: string | null;
    onSelectCheckpoint: (userItemId: string | null) => void;
    onJumpToCheckpoint: () => void;
  }

  let {
    visibleCheckpoints,
    selectedUserItemId,
    onSelectCheckpoint,
    onJumpToCheckpoint,
  }: Props = $props();
</script>

<div class="flex min-w-0 items-stretch border-t border-border-subtle">
  <div class="min-w-0 flex-1 overflow-x-auto px-3 py-2" data-testid="diff-message-scroll-strip">
    <div class="flex w-max gap-1">
      <button
        class="shrink-0 rounded border px-2.5 py-1 text-[12px] {selectedUserItemId === null ? 'border-accent/60 bg-accent/15 text-accent' : 'border-border-subtle text-fg-muted hover:bg-surface-2'}"
        onclick={() => onSelectCheckpoint(null)}
        aria-label="All messages"
        data-testid="diff-all-messages"
      >
        All
      </button>
      {#each visibleCheckpoints as checkpoint (checkpoint.id)}
        <button
          class="min-w-[30px] shrink-0 rounded border px-2 py-1 text-center text-[12px] {selectedUserItemId === checkpoint.userItemId ? 'border-accent/60 bg-accent/15 text-accent' : 'border-border-subtle text-fg-muted hover:bg-surface-2'}"
          onclick={() => onSelectCheckpoint(checkpoint.userItemId)}
          aria-label={`Message ${checkpoint.turnIndex + 1}`}
          data-testid={`diff-message-${checkpoint.turnIndex}`}
        >
          {checkpoint.turnIndex + 1}
          {#if checkpoint.status && checkpoint.status !== 'ready'}
            <span class="ml-1 text-[10px] text-warning">{checkpoint.status}</span>
          {/if}
        </button>
      {/each}
    </div>
  </div>
  <div class="flex shrink-0 items-center border-l border-border-subtle bg-surface-1/70 px-2" data-testid="diff-message-jump-slot">
    <button
      class="inline-flex h-[28px] w-[30px] items-center justify-center rounded border border-border-subtle text-fg-muted transition-colors hover:bg-surface-2 hover:text-fg disabled:cursor-default disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-fg-muted"
      onclick={onJumpToCheckpoint}
      disabled={selectedUserItemId === null}
      title="Jump to message"
      aria-label="Jump to message"
      data-testid="diff-message-jump"
    >
      <Icon icon={LocateFixed} size={13} />
    </button>
  </div>
</div>
