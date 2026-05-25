<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import { getThreads } from '../../stores/threads.svelte';
  import { openThreadFromNavigation } from '../../stores/panes.svelte';
  import {
    getEffectiveThreadStatus,
  } from '../../stores/threadStatuses.svelte';
  import { computeHighlightSegments } from '../../utils/highlight';
  import { pathBasename } from '../../utils/pathDisplay';
  import { getProviderDefinition } from '../../providers/catalog';
  import { resolveThreadStatusPill } from '../../utils/threadStatusPill';
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
  let activeIndex = $state(0);

  // Result cap — above this, users should refine their query. The footer
  // surfaces the overflow count so nothing is silently dropped.
  const LIMIT = 50;

  interface Ranked {
    thread: Thread;
    // 0 = prefix match on title, 1 = substring match on title, 2 = match on
    // workspace/project path only. Lower ranks sort first.
    rank: number;
  }

  // Non-archived threads are the only candidates. Archived threads live
  // behind their own filter in the sidebar and shouldn't clutter the picker.
  let visibleThreads: Thread[] = $derived(getThreads().filter((t) => !t.archived));

  let matches: Ranked[] = $derived.by(() => {
    const q = query.trim().toLowerCase();
    if (q.length === 0) return visibleThreads.map((thread) => ({ thread, rank: 1 }));
    const out: Ranked[] = [];
    for (const thread of visibleThreads) {
      const title = (thread.title || '').toLowerCase();
      const ws = (thread.workspacePath || '').toLowerCase();
      const proj = (thread.projectPath || '').toLowerCase();
      if (title.startsWith(q)) out.push({ thread, rank: 0 });
      else if (title.includes(q)) out.push({ thread, rank: 1 });
      else if (ws.includes(q) || proj.includes(q)) out.push({ thread, rank: 2 });
    }
    // Stable sort by rank so prefix hits bubble to the top.
    out.sort((a, b) => a.rank - b.rank);
    return out;
  });

  let hits: Ranked[] = $derived(matches.slice(0, LIMIT));
  let overflow = $derived(Math.max(0, matches.length - hits.length));

  // Reset query + selection every time the picker opens so yesterday's
  // search doesn't linger. Focus the list root by default so plain j/k
  // navigates; mod+/ toggles to the search input.
  $effect(() => {
    if (open) {
      query = '';
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

  // Clamp the active index whenever the hit list shrinks.
  $effect(() => {
    if (hits.length === 0) activeIndex = 0;
    else if (activeIndex >= hits.length) activeIndex = hits.length - 1;
    else if (activeIndex < 0) activeIndex = 0;
  });

  async function openHit(thread: Thread): Promise<void> {
    onClose();
    await openThreadFromNavigation(thread, pane);
  }

  // Escape is handled by Modal. Enter / Arrow keys are caught at the
  // body level so the search input keeps getting keystrokes it cares
  // about (letters) while navigation / activation are intercepted.
  // j/k aliases work only when the list root has focus — otherwise
  // they'd type into the search input.
  function handleBodyKeydown(e: KeyboardEvent): void {
    if (hits.length === 0) return;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      activeIndex = (activeIndex + 1) % hits.length;
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      activeIndex = (activeIndex - 1 + hits.length) % hits.length;
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const hit = hits[activeIndex];
      if (hit) void openHit(hit.thread);
    } else if (
      document.activeElement === listEl &&
      !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey
    ) {
      if (e.key === 'j') {
        e.preventDefault();
        activeIndex = (activeIndex + 1) % hits.length;
      } else if (e.key === 'k') {
        e.preventDefault();
        activeIndex = (activeIndex - 1 + hits.length) % hits.length;
      }
    }
  }
</script>

<Modal
  {open}
  title="Jump to thread"
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
      data-testid="thread-picker"
      tabindex={-1}
      class="focus:outline-none"
      onkeydown={handleBodyKeydown}
    >
      <input
        bind:this={searchEl}
        bind:value={query}
        type="text"
        placeholder="Filter by title, project, or workspace…"
        aria-label="Filter threads"
        data-testid="thread-picker-input"
        class="w-full text-[0.8125rem] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors mb-3"
      />

      {#if visibleThreads.length === 0}
        <div class="px-2 py-3 text-[0.75rem] text-fg-muted" data-testid="thread-picker-empty">
          No threads yet — create one from the sidebar.
        </div>
      {:else if hits.length === 0}
        <div class="px-2 py-3 text-[0.75rem] text-fg-muted" data-testid="thread-picker-empty">
          No threads match "{query.trim()}".
        </div>
      {:else}
        <ul class="py-1 -mx-1" data-testid="thread-picker-results">
          {#each hits as hit, i (hit.thread.id)}
            {@const status = getEffectiveThreadStatus(hit.thread)}
            {@const statusPill = resolveThreadStatusPill(hit.thread, status)}
            {@const basename = pathBasename(hit.thread.projectPath)}
            {@const providerDefinition = getProviderDefinition(hit.thread.provider)}
            <li>
              <button
                type="button"
                data-testid="thread-picker-hit-{hit.thread.id}"
                onclick={() => openHit(hit.thread)}
                aria-current={activeIndex === i}
                class={[
                  'w-full text-left px-3 py-1.5 flex items-center gap-2 cursor-pointer transition-colors rounded-[var(--radius-field)]',
                  activeIndex === i ? 'bg-accent/10 text-fg' : 'hover:bg-surface-2/30 text-fg-muted',
                ].join(' ')}
              >
                <span class="text-[0.5625rem] font-semibold px-1 py-0.5 rounded-[4px] shrink-0 tracking-wide
                    {providerDefinition?.badgeClass ?? 'bg-surface-2 text-fg-muted'}" aria-hidden="true">
                  {providerDefinition?.shortLabel ?? '?'}
                </span>
                {#if !statusPill}
                  <span class="w-2 h-2 shrink-0" aria-hidden="true"></span>
                {:else}
                  <span
                    class="w-2 h-2 rounded-full shrink-0 {statusPill.dotClass} {statusPill.pulse ? 'animate-pulse' : ''}"
                    role="status"
                    aria-label={statusPill.label}
                    title={statusPill.label}
                    data-testid="thread-picker-status-dot"
                    data-status={status}
                  ></span>
                {/if}
                <span class="text-[0.8125rem] truncate text-fg flex-1 min-w-0">
                  {#each computeHighlightSegments(hit.thread.title || 'Untitled', query) as seg}
                    {#if seg.type === 'match'}
                      <mark class="bg-accent/30 text-fg rounded-sm px-0.5">{seg.value}</mark>
                    {:else}{seg.value}{/if}
                  {/each}
                </span>
                {#if hit.thread.worktreePath}
                  <span class="text-[0.5625rem] px-1 py-0.5 rounded-[4px] bg-accent/10 text-accent/80 shrink-0" title="Worktree: {hit.thread.worktreePath}">worktree</span>
                {/if}
                {#if basename}
                  <span class="text-[0.625rem] text-fg-hint shrink-0 ml-auto truncate max-w-[12rem] font-mono">{basename}</span>
                {/if}
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <div class="flex items-center justify-between gap-3 text-[0.625rem] text-fg-hint w-full">
      <span>↑↓ to navigate · ↵ to open · Esc to close</span>
      {#if overflow > 0}
        <span data-testid="thread-picker-overflow" class="text-fg-muted tabular-nums">
          {overflow} more — refine your query.
        </span>
      {/if}
    </div>
  {/snippet}
</Modal>
