<script lang="ts">
  // Right-edge hover action buttons for a thread row. Extracted from
  // ThreadRow.svelte to keep the row under 300 lines. Each button is a
  // pure onclick forwarder — no local state, no fetches.

  import type { Thread } from '../../types/models';

  let {
    thread,
    onStartDiscussion,
    canStartDiscussion,
    onFork,
    onDelete,
    onArchive,
    onUnarchive,
  }: {
    thread: Thread;
    onStartDiscussion?: (e: MouseEvent) => void;
    canStartDiscussion: boolean;
    onFork: (e: MouseEvent) => void;
    onDelete: (e: MouseEvent) => void;
    onArchive: (e: MouseEvent) => void;
    onUnarchive: (e: MouseEvent) => void;
  } = $props();
</script>

{#if onStartDiscussion && canStartDiscussion}
  <button
    onclick={onStartDiscussion}
    class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
    aria-label="Start discussion on thread"
    title="Start discussion"
  >
    <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
    </svg>
  </button>
{/if}
<button
  onclick={onFork}
  disabled={!thread.sessionRef}
  class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer disabled:cursor-not-allowed disabled:opacity-0 focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
  aria-label="Fork thread"
  title={thread.sessionRef ? 'Fork thread' : 'Fork available after the thread has provider state'}
>
  <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <circle cx="6" cy="6" r="2" />
    <circle cx="18" cy="6" r="2" />
    <circle cx="18" cy="18" r="2" />
    <path d="M8 6h7" />
    <path d="M18 8v8" />
    <path d="M8 7.5c4 1 7 4 8 8" />
  </svg>
</button>
<button
  onclick={onDelete}
  class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-error/60 hover:text-error text-xs px-1 shrink-0 cursor-pointer focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
  aria-label="Delete thread"
>
  <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
  </svg>
</button>
{#if thread.archived}
  <button
    onclick={onUnarchive}
    data-testid="thread-row-unarchive"
    class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
    aria-label="Unarchive thread"
    title="Unarchive thread"
  >
    <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M21 8v13H3V8M1 3h22v5H1z" />
      <path d="M9 12h6" />
      <path d="M12 9v6" />
    </svg>
  </button>
{:else}
  <button
    onclick={onArchive}
    data-testid="thread-row-archive"
    class="opacity-0 group-hover:opacity-100 transition-opacity duration-150 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50 rounded"
    aria-label="Archive thread"
  >
    <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M21 8v13H3V8M1 3h22v5H1zM10 12h4" />
    </svg>
  </button>
{/if}
