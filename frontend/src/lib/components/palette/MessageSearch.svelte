<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import { SearchThreadMessages, SearchThreadItems, type ThreadMessageHit } from '../../stores/bindings';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { MessageSearchMode } from '../../stores/messageSearch.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { openThreadFromNavigation } from '../../stores/panes.svelte';
  import { getProviderDefinition } from '../../providers/catalog';
  import { asProviderID, type ProviderID } from '../../types/providers';
  import { computeHighlightSegments } from '../../utils/highlight';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { PICKER_TOGGLE_INPUT_EVENT } from '../../stores/events';

  interface Props {
    open: boolean;
    pane: ThreadPane | null;
    mode: MessageSearchMode;
    onClose: () => void;
  }

  let { open, pane, mode, onClose }: Props = $props();

  // 'thread' searches only the target pane's thread (mod+f, in-thread find);
  // 'global' searches every thread's titles + messages (mod+shift+f).
  const dialogTitle = $derived(mode === 'thread' ? 'Find in thread' : 'Search messages');
  const inputPlaceholder = $derived(
    mode === 'thread' ? 'Find in this thread…' : 'Search titles and message text…',
  );
  const idleHint = $derived(
    mode === 'thread'
      ? 'Type to find messages in this thread.'
      : 'Type to search across thread titles and message text.',
  );

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

  // Debounce window for re-querying as the user types. The global search is a
  // full table scan; firing one per keystroke piled scans onto the store's
  // single write connection during a live stream. One scan per typing pause is
  // plenty for an interactive dialog. (In-thread find is cheap by comparison,
  // but shares the path for consistency.)
  const SEARCH_DEBOUNCE_MS = 150;

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
      let result: ThreadMessageHit[];
      if (mode === 'thread') {
        // In-thread find is scoped to the open thread; with no thread loaded
        // (a draft pane) there is nothing to search.
        result = pane?.threadId
          ? await SearchThreadItems(pane.threadId, trimmed, LIMIT)
          : [];
      } else {
        result = await SearchThreadMessages(trimmed, LIMIT);
      }
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

  // Re-run (debounced) when the query changes OR when the dialog opens. The
  // searchSeq guard inside runSearch still discards any stale response that
  // lands out of order; debounce additionally avoids issuing the scans at all.
  let debounceTimer: ReturnType<typeof setTimeout> | undefined;
  $effect(() => {
    if (!open) return;
    const q = query;
    clearTimeout(debounceTimer);
    // An empty query is a local clear — nothing to debounce.
    if (q.trim().length === 0) {
      void runSearch(q);
      return;
    }
    debounceTimer = setTimeout(() => void runSearch(q), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(debounceTimer);
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
    // a nonce the live MessageTimeline observes and handles by loading the
    // item into the window and jumping via the virtualizer's scrollToIndex
    // (an escaped, controller-routed write). Title-match hits have no
    // itemId; those stop after the thread switch without further navigation.
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
      // Mid-IME-composition Enter confirms the candidate in the search input;
      // opening the highlighted hit would navigate on a half-typed query.
      if (isImeComposingEvent(e)) return;
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

{#snippet highlighted(text: string)}
  {#each computeHighlightSegments(text, query) as seg}
    {#if seg.type === 'match'}<mark class="bg-accent/30 text-fg rounded-sm px-0.5">{seg.value}</mark>{:else}{seg.value}{/if}
  {/each}
{/snippet}

<Modal
  {open}
  title={dialogTitle}
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
        placeholder={inputPlaceholder}
        aria-label={dialogTitle}
        data-testid="message-search-input"
        class="w-full text-[0.8125rem] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors mb-3"
      />

      {#if loading}
        <div class="px-2 py-3 text-[0.75rem] text-fg-muted" data-testid="message-search-loading">Searching…</div>
      {:else if error}
        <div class="px-2 py-3 text-[0.75rem] text-error" data-testid="message-search-error">{error}</div>
      {:else if query.trim().length === 0}
        <div class="px-2 py-3 text-[0.75rem] text-fg-muted" data-testid="message-search-idle">
          {idleHint}
        </div>
      {:else if hits.length === 0}
        <div class="px-2 py-3 text-[0.75rem] text-fg-muted" data-testid="message-search-empty">
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
                {#if mode === 'thread'}
                  <!-- In-thread find: every hit is a message in the current
                       thread, so drop the redundant provider + title and show
                       the matching text with just a turn marker. -->
                  <div class="flex items-baseline gap-2">
                    <span class="text-[0.5625rem] px-1 py-0.5 rounded-[4px] bg-surface-0 border border-border-subtle text-fg-hint shrink-0 tabular-nums">
                      turn {hit.turnIndex}
                    </span>
                    <span class="text-[0.8125rem] truncate text-fg-muted">{@render highlighted(hit.summary)}</span>
                  </div>
                {:else}
                  <div class="flex items-center gap-2">
                    <span class="text-[0.5625rem] font-semibold px-1 py-0.5 rounded-[4px] shrink-0 tracking-wide
                      {providerDefinition?.badgeClass ?? 'bg-surface-2 text-fg-muted'}" aria-hidden="true">
                      {providerDefinition?.shortLabel ?? '?'}
                    </span>
                    <span class="text-[0.8125rem] truncate text-fg">{@render highlighted(hit.threadTitle || 'Untitled')}</span>
                    <span class="text-[0.5625rem] ml-auto px-1 py-0.5 rounded-[4px] bg-surface-0 border border-border-subtle text-fg-hint shrink-0 tabular-nums">
                      {hit.matchType === 'title' ? 'title' : `turn ${hit.turnIndex}`}
                    </span>
                  </div>
                  {#if hit.matchType === 'item' && hit.summary}
                    <p class="text-[0.75rem] text-fg-muted truncate">{@render highlighted(hit.summary)}</p>
                  {/if}
                {/if}
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <span class="text-[0.625rem] text-fg-hint w-full">↑↓ to navigate · ↵ to open · Esc to close</span>
  {/snippet}
</Modal>
