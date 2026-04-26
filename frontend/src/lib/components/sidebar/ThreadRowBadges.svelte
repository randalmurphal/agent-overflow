<script lang="ts">
  // Trailing badges for a non-editing thread row: discussion / design /
  // worktree indicators. Fork lineage lives in ThreadRow's left metadata
  // cluster because it behaves like a navigation affordance, not a badge.

  import type { Thread } from '../../types/models';

  let {
    thread,
  }: {
    thread: Thread;
  } = $props();
</script>

{#if thread.mode === 'discussion'}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-accent/10 text-accent shrink-0" title="Discussion Parent Thread" aria-label="Discussion Parent Thread">D</span>
{:else if thread.parentThreadId}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-provider-codex/10 text-provider-codex shrink-0" title="Discussion Participant" aria-label="Discussion Participant">Dp</span>
{:else if thread.mode === 'design'}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-provider-codex/10 text-provider-codex shrink-0" title="Design Mode Thread" aria-label="Design Mode Thread">Dsn</span>
{/if}
{#if thread.worktreePath}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-accent/10 text-accent/80 shrink-0" title="Worktree: {thread.worktreePath}">WT</span>
{/if}
