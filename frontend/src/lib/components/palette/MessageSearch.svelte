<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import { SearchThreadMessages, type ThreadMessageHit } from '../../stores/bindings';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { openThreadFromNavigation } from '../../stores/panes.svelte';
  import { getProviderDefinition } from '../../providers/catalog';
  import { asProviderID, type ProviderID } from '../../types/providers';
  import { computeHighlightSegments } from '../../utils/highlight';
  import { PICKER_TOGGLE_INPUT_EVENT } from '../../stores/events';

  interface Props {
    open: boolean;
    pane: ThreadPane | null;
    onClose: () => void;
  }

  let { open, pane, onClose }: Props = $props();

  let query = $state('');
  let searchEl: HTMLInputElement | undefined = $state(undefined);
  let listEl: HTMLDivElement | undefined = $state(undefined);
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
      // Reset state on each open so the sheet doesn't show yesterday's
      // query. List root is focused by default so plain j/k navigates;
      // mod+/ toggles focus to the search input.
      query = '';
      hits = [];
      error = null;
      activeIndex = 0;
      requestAnimationFrame(() => listEl?.focus());
    }
  });

  $effect(() => {
    if (!open || typeof window === 'undefined') return;
    const handler = (): void => {
      if (document.activeElement === searchEl) {
        listEl?.focus();
      } else {
        searchEl?.focus();
        searchEl?.select();
      }
    };
    window.addEventListener(PICKER_TOGGLE_INPUT_EVENT, handler);
    return () => window.removeEventListener(PICKER_TOGGLE_INPUT_EVENT, handler);
  });

  async function openHit(hit: ThreadMessageHit): Promise<void> {
    // The sidebar may not have the full thread row loaded (e.g. archived);
    // fall back to a minimal shape so switchThread still navigates.
    const thread = getThreadById(hit.threadId) ?? ({
      id: hit.threadId,
      title: hit.threadTitle,
      provider: (asProviderID(hit.provider) ?? hit.provider) as ProviderID,
      workspacePath: '',
      projectPath: '',
      mode: 'chat' as const,
      model: '',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    });
    onClose();
    const targetPane = await openThreadFromNavigation(thread, pane);
    // Scroll the timeline to the hit — pane.requestScrollToItem publishes
    // a nonce the live MessageTimeline observes and handles via
    // loadUntilItem + scrollIntoView. Title-match hits have no itemId;
    // those stop after the thread switch without further navigation.
    if (hit.matchType === 'item' && hit.itemId) {
      targetPane.requestScrollToItem(hit.itemId);
    }
  }

  // Escape is handled by Modal. Arrow / Enter keys are caught at the
  // body level so the search input keeps receiving alphanumeric keys.
  // j/k aliases work only when the list root has focus.
  function handleBodyKeydown(e: KeyboardEvent): void {
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
      return;
    }
    if (
      document.activeElement === listEl &&
      !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey
    ) {
      if (e.key === 'j') {
        e.preventDefault();
        if (hits.length === 0) return;
        activeIndex = (activeIndex + 1) % hits.length;
      } else if (e.key === 'k') {
        e.preventDefault();
        if (hits.length === 0) return;
        activeIndex = (activeIndex - 1 + hits.length) % hits.length;
      }
    }
  }
</script>

<Modal
  {open}
  title="Search messages"
  onClose={onClose}
  width="lg"
  padding="tight"
  align="top"
>
  {#snippet children()}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div
      bind:this={listEl}
      data-testid="message-search"
      tabindex={-1}
      class="focus:outline-none"
      onkeydown={handleBodyKeydown}
    >
      <input
        bind:this={searchEl}
        bind:value={query}
        type="text"
        placeholder="Search titles and message text…"
        aria-label="Search messages"
        data-testid="message-search-input"
        class="w-full text-[13px] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors mb-3"
      />

      {#if loading}
        <div class="px-2 py-3 text-[12px] text-fg-muted" data-testid="message-search-loading">Searching…</div>
      {:else if error}
        <div class="px-2 py-3 text-[12px] text-error" data-testid="message-search-error">{error}</div>
      {:else if query.trim().length === 0}
        <div class="px-2 py-3 text-[12px] text-fg-muted" data-testid="message-search-idle">
          Type to search across thread titles and message text.
        </div>
      {:else if hits.length === 0}
        <div class="px-2 py-3 text-[12px] text-fg-muted" data-testid="message-search-empty">
          No matches for "{query.trim()}".
        </div>
      {:else}
        <ul class="py-1 -mx-1" data-testid="message-search-results">
          {#each hits as hit, i (hit.threadId + ':' + (hit.itemId || 'title'))}
            {@const providerDefinition = getProviderDefinition(hit.provider)}
            <li>
              <button
                type="button"
                data-testid="message-search-hit-{hit.threadId}-{hit.itemId || 'title'}"
                onclick={() => openHit(hit)}
                aria-current={activeIndex === i}
                class={[
                  'w-full text-left px-3 py-1.5 flex flex-col gap-0.5 cursor-pointer transition-colors rounded-[var(--radius-field)]',
                  activeIndex === i ? 'bg-accent/10 text-fg' : 'hover:bg-surface-2/30 text-fg-muted',
                ].join(' ')}
              >
                <div class="flex items-center gap-2">
                  <span class="text-[9px] font-semibold px-1 py-0.5 rounded-[4px] shrink-0 tracking-wide
                    {providerDefinition?.badgeClass ?? 'bg-surface-2 text-fg-muted'}" aria-hidden="true">
                    {providerDefinition?.shortLabel ?? '?'}
                  </span>
                  <span class="text-[13px] truncate text-fg">
                    {#each computeHighlightSegments(hit.threadTitle || 'Untitled', query) as seg}
                      {#if seg.type === 'match'}
                        <mark class="bg-accent/30 text-fg rounded-sm px-0.5">{seg.value}</mark>
                      {:else}
                        {seg.value}
                      {/if}
                    {/each}
                  </span>
                  <span class="text-[9px] ml-auto px-1 py-0.5 rounded-[4px] bg-surface-0 border border-border-subtle text-fg-hint shrink-0 tabular-nums">
                    {hit.matchType === 'title' ? 'title' : `turn ${hit.turnIndex}`}
                  </span>
                </div>
                {#if hit.matchType === 'item' && hit.summary}
                  <p class="text-[12px] text-fg-muted truncate">
                    {#each computeHighlightSegments(hit.summary, query) as seg}
                      {#if seg.type === 'match'}
                        <mark class="bg-accent/30 text-fg rounded-sm px-0.5">{seg.value}</mark>
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
  {/snippet}
  {#snippet footer()}
    <span class="text-[10px] text-fg-hint w-full">↑↓ to navigate · ↵ to open · Esc to close</span>
  {/snippet}
</Modal>
