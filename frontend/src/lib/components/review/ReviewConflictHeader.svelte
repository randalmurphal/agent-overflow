<script lang="ts">
  // The conflict view's header: what it merges, its Back (unless the host's
  // header owns Back), and the merge-tree messages that name no path.
  import { hostedBackReader } from '../panes/hostedBack';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';

  let { review }: { review: ReviewPaneState } = $props();

  const hostOwnsBack = hostedBackReader();
</script>

<div class="flex items-center justify-between gap-3 border-b border-border bg-surface-1 px-3 py-2 text-xs">
  <span class="min-w-0 truncate text-fg-muted">
    {#if review.conflicts}
      Conflicts: {review.conflicts.baseLabel} ← {review.conflicts.headLabel} — read-only
    {:else}
      Conflicts — read-only
    {/if}
  </span>
  {#if !hostOwnsBack()}
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:text-fg"
      data-testid="review-conflicts-back"
      onclick={() => review.closeConflictView()}
    >
      Back
    </button>
  {/if}
</div>
{#if review.conflicts && review.conflicts.messages.length > 0}
  <!-- Fallback only: per-file messages render as note rows inside
       their file's body; this strip carries the rare merge-tree
       message that names no conflicted path. -->
  <div
    class="max-h-28 overflow-y-auto border-b border-warning/30 bg-warning/5 px-3 py-2 font-mono text-[0.6875rem] leading-relaxed text-fg-muted whitespace-pre-wrap"
    data-testid="review-conflict-messages"
  >{review.conflicts.messages.join('\n')}</div>
{/if}
