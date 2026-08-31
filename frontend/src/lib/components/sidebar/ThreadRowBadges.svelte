<script lang="ts">
  // Trailing badges for discussion threads. Fork lineage lives in ThreadRow's left metadata cluster
  // because it behaves like a navigation affordance, not a badge.
  //
  // Terminal threads are NOT badged here: they render a LEADING green
  // terminal glyph in ThreadRow (the `thread-row-terminal-icon` span — the
  // boxless `>_` matching the chat-history bash tool-call icon), mirroring the
  // draft icon's slot, so they read apart from chat rows from the left edge
  // rather than via a trailing badge.

  import type { Thread } from '../../types/models';

  let {
    thread,
  }: {
    thread: Thread;
  } = $props();
</script>

{#if thread.mode === 'discussion'}
  <span class="text-[0.53125rem] px-1 py-[1px] rounded-[4px] bg-accent/10 text-accent shrink-0" title="Discussion Parent Thread" aria-label="Discussion Parent Thread">D</span>
{:else if thread.parentThreadId}
  <span class="text-[0.53125rem] px-1 py-[1px] rounded-[4px] bg-provider-codex/10 text-provider-codex shrink-0" title="Discussion Participant" aria-label="Discussion Participant">Dp</span>
{/if}
