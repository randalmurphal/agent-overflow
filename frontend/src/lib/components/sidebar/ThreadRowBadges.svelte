<script lang="ts">
  // Trailing badges for a non-editing thread row: discussion / design /
  // worktree / fork-lineage indicators. Extracted from ThreadRow.svelte
  // to keep the shell under 300 lines.

  import type { Thread } from '../../types/models';

  let {
    thread,
    forkParent,
    onJumpToParent,
  }: {
    thread: Thread;
    forkParent: Thread | undefined;
    onJumpToParent: (e: MouseEvent) => void;
  } = $props();
</script>

{#if thread.mode === 'discussion'}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-accent/10 text-accent shrink-0" title="Discussion parent thread" aria-label="Discussion parent thread">D</span>
{:else if thread.parentThreadId}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-provider-codex/10 text-provider-codex shrink-0" title="Discussion participant" aria-label="Discussion participant">Dp</span>
{:else if thread.mode === 'design'}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-provider-codex/10 text-provider-codex shrink-0" title="Design mode thread" aria-label="Design mode thread">Dsn</span>
{/if}
{#if thread.worktreePath}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-accent/10 text-accent/80 shrink-0" title="Worktree: {thread.worktreePath}">WT</span>
{/if}
{#if thread.forkedFromThreadId}
  <button
    type="button"
    data-testid="thread-row-fork-lineage"
    onclick={onJumpToParent}
    disabled={!forkParent}
    class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-provider-codex/10 text-provider-codex shrink-0 cursor-pointer disabled:cursor-not-allowed disabled:opacity-60 hover:bg-provider-codex/20 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40 transition-colors"
    title={forkParent
      ? `Forked from "${forkParent.title || 'Untitled'}" — click to open parent`
      : 'Forked thread (parent not loaded in sidebar)'}
    aria-label="Fork lineage"
  >
    F↩
  </button>
{/if}
