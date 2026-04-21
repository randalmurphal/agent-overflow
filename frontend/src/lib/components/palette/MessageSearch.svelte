<script lang="ts">
  import { fade, scale } from 'svelte/transition';
  import { focusTrap } from '../../utils/focusTrap';
  import { SearchThreadMessages, type ThreadMessageHit } from '../../stores/bindings';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { computeHighlightSegments } from '../../utils/highlight';

  interface Props {
    open: boolean;
    pane: ThreadPane;
    onClose: () => void;
  }

  let { open, pane, onClose }: Props = $props();

  let query = $state('');
  let searchEl: HTMLInputElement | undefined = $state(undefined);
  let hits: ThreadMessageHit[] = $state([]);
  let loading = $state(false);
  let error: string | null = $state(null);
  let activeIndex = $state(0);

  // Result limit — deliberately modest to keep the dialog scannable. Users
  // with >50 matches should narrow their query.
  const LIMIT = 50;

  // Re-query on every change to `query` while the dialog is open. SQLite LIKE
  // is fast enough here (thousands of items) that per-keystroke search feels
  // instant; a future FTS5 migration would preserve this contract.
  let searchSeq = 0;
  async function runSearch(q: string): Promise<void> {
    const seq = ++searchSeq;
    const trimmed = q.trim();
    if (trimmed.length === 0) {
      hits = [];
      error = null;
      loading = false;
      activeIndex = 0;
      return;
    }
    loading = true;
    error = null;
    try {
      const result = await SearchThreadMessages(trimmed, LIMIT);
      // Ignore stale responses — user may have typed more since.
      if (seq !== searchSeq) return;
      hits = result ?? [];
      activeIndex = hits.length > 0 ? 0 : -1;
    } catch (err) {
      if (seq !== searchSeq) return;
      error = err instanceof Error ? err.message : String(err);
      hits = [];
    } finally {
      if (seq === searchSeq) loading = false;
    }
  }

  // Re-run when the query changes OR when the dialog opens.
  $effect(() => {
    if (open) void runSearch(query);
  });

  $effect(() => {
    if (open) {
      // Reset state on each open so the sheet doesn't show yesterday's query.
      query = '';
      hits = [];
      error = null;
      activeIndex = 0;
      requestAnimationFrame(() => searchEl?.focus());
    }
  });

  async function openHit(hit: ThreadMessageHit): Promise<void> {
    // The sidebar may not have the full thread row loaded (e.g. archived);
    // fall back to a minimal shape so switchThread still navigates. The
    // provider string is opaque from the store's perspective, so we cast
    // to the Thread union here — switchThread only reads the fields we fill.
    const thread = getThreadById(hit.threadId) ?? ({
      id: hit.threadId,
      title: hit.threadTitle,
      provider: hit.provider as 'claude' | 'codex',
      workspacePath: '',
      projectPath: '',
      mode: 'chat' as const,
      model: '',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    });
    onClose();
    await pane.switchThread(thread);
    // Scroll the timeline to the hit — pane.requestScrollToItem publishes
    // a nonce the live MessageTimeline observes and handles via
    // loadUntilItem + scrollIntoView. Title-match hits have no itemId;
    // those stop after the thread switch without further navigation.
    if (hit.matchType === 'item' && hit.itemId) {
      pane.requestScrollToItem(hit.itemId);
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
      return;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (hits.length === 0) return;
      activeIndex = (activeIndex + 1) % hits.length;
      return;
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (hits.length === 0) return;
      activeIndex = (activeIndex - 1 + hits.length) % hits.length;
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (activeIndex >= 0 && activeIndex < hits.length) {
        void openHit(hits[activeIndex]);
      }
    }
  }

  function handleBackdropClick(e: MouseEvent): void {
    if (e.target === e.currentTarget) onClose();
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    transition:fade={{ duration: 150 }}
    class="fixed inset-0 z-[60] flex items-start justify-center pt-16 bg-overlay backdrop-blur-sm"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
    data-testid="message-search-backdrop"
  >
    <div
      use:focusTrap={{ active: open }}
      transition:scale={{ start: 0.95, duration: 150 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="message-search-title"
      data-testid="message-search"
      class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-2xl w-full mx-4 max-h-[80vh] flex flex-col"
    >
      <div class="px-5 pt-5 pb-3 border-b border-border">
        <h2 id="message-search-title" class="text-base font-semibold text-text-primary">
          Search messages
        </h2>
        <input
          bind:this={searchEl}
          bind:value={query}
          type="text"
          placeholder="Search titles and message text…"
          aria-label="Search messages"
          data-testid="message-search-input"
          class="mt-3 w-full text-sm rounded border border-border bg-surface-0 px-2.5 py-1.5 text-text-primary focus:outline-none focus:ring-2 focus:ring-accent/50"
        />
      </div>

      <div class="flex-1 overflow-y-auto">
        {#if loading}
          <div class="px-5 py-4 text-xs text-text-secondary" data-testid="message-search-loading">Searching…</div>
        {:else if error}
          <div class="px-5 py-4 text-xs text-error" data-testid="message-search-error">{error}</div>
        {:else if query.trim().length === 0}
          <div class="px-5 py-4 text-xs text-text-secondary" data-testid="message-search-idle">
            Type to search across thread titles and message text.
          </div>
        {:else if hits.length === 0}
          <div class="px-5 py-4 text-xs text-text-secondary" data-testid="message-search-empty">
            No matches for "{query.trim()}".
          </div>
        {:else}
          <ul class="py-1" data-testid="message-search-results">
            {#each hits as hit, i (hit.threadId + ':' + (hit.itemId || 'title'))}
              <li>
                <button
                  type="button"
                  data-testid="message-search-hit-{hit.threadId}-{hit.itemId || 'title'}"
                  onclick={() => openHit(hit)}
                  aria-current={activeIndex === i}
                  class={[
                    'w-full text-left px-5 py-2 flex flex-col gap-0.5 cursor-pointer transition-colors',
                    activeIndex === i ? 'bg-accent/15 text-text-primary' : 'hover:bg-surface-2/50 text-text-secondary',
                  ].join(' ')}
                >
                  <div class="flex items-center gap-2">
                    <span class="text-[10px] font-bold px-1 py-0.5 rounded shrink-0
                      {hit.provider === 'claude' ? 'bg-accent/20 text-accent' : 'bg-provider-codex/20 text-provider-codex'}" aria-hidden="true">
                      {hit.provider === 'claude' ? 'C' : 'X'}
                    </span>
                    <span class="text-sm truncate text-text-primary">
                      {#each computeHighlightSegments(hit.threadTitle || 'Untitled', query) as seg}
                        {#if seg.type === 'match'}
                          <mark class="bg-accent/30 text-text-primary rounded-sm px-0.5">{seg.value}</mark>
                        {:else}
                          {seg.value}
                        {/if}
                      {/each}
                    </span>
                    <span class="text-[9px] ml-auto px-1 py-0.5 rounded bg-surface-0 border border-border text-text-secondary shrink-0">
                      {hit.matchType === 'title' ? 'title' : `turn ${hit.turnIndex}`}
                    </span>
                  </div>
                  {#if hit.matchType === 'item' && hit.summary}
                    <p class="text-xs text-text-secondary/80 truncate">
                      {#each computeHighlightSegments(hit.summary, query) as seg}
                        {#if seg.type === 'match'}
                          <mark class="bg-accent/30 text-text-primary rounded-sm px-0.5">{seg.value}</mark>
                        {:else}
                          {seg.value}
                        {/if}
                      {/each}
                    </p>
                  {/if}
                </button>
              </li>
            {/each}
          </ul>
        {/if}
      </div>

      <div class="px-5 py-2 border-t border-border text-[10px] text-text-secondary/70">
        ↑↓ to navigate · ↵ to open · Esc to close
      </div>
    </div>
  </div>
{/if}
