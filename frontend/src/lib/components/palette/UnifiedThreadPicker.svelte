<script lang="ts">
  import { fade, scale } from 'svelte/transition';
  import { focusTrap } from '../../utils/focusTrap';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import { getThreads } from '../../stores/threads.svelte';
  import { getThreadStatus, type ThreadLiveStatus } from '../../stores/threadStatuses.svelte';
  import { computeHighlightSegments } from '../../utils/highlight';

  interface Props {
    open: boolean;
    pane: ThreadPane;
    onClose: () => void;
  }

  let { open, pane, onClose }: Props = $props();

  let query = $state('');
  let searchEl: HTMLInputElement | undefined = $state(undefined);
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
  // search doesn't linger.
  $effect(() => {
    if (open) {
      query = '';
      activeIndex = 0;
      requestAnimationFrame(() => searchEl?.focus());
    }
  });

  // Clamp the active index whenever the hit list shrinks.
  $effect(() => {
    if (hits.length === 0) activeIndex = 0;
    else if (activeIndex >= hits.length) activeIndex = hits.length - 1;
    else if (activeIndex < 0) activeIndex = 0;
  });

  async function openHit(thread: Thread): Promise<void> {
    onClose();
    await pane.switchThread(thread);
  }

  function projectBasename(path: string): string {
    if (!path) return '';
    // Strip trailing slash so a path like "/foo/bar/" doesn't split into an
    // empty final segment.
    const parts = path.replace(/\/+$/, '').split('/');
    return parts[parts.length - 1] ?? '';
  }

  // Status dot styling is identical to ThreadRow so the picker stays
  // visually consistent with the sidebar.
  const STATUS_CLASS: Record<ThreadLiveStatus, string> = {
    idle: '',
    running: 'bg-warning animate-pulse',
    'pending-approval': 'bg-accent',
    error: 'bg-error',
  };
  const STATUS_LABEL: Record<ThreadLiveStatus, string> = {
    idle: '',
    running: 'Running',
    'pending-approval': 'Pending approval',
    error: 'Error',
  };

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
      return;
    }
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
    data-testid="thread-picker-backdrop"
  >
    <div
      use:focusTrap={{ active: open }}
      transition:scale={{ start: 0.95, duration: 150 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="thread-picker-title"
      data-testid="thread-picker"
      class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-2xl w-full mx-4 max-h-[80vh] flex flex-col"
    >
      <div class="px-5 pt-5 pb-3 border-b border-border">
        <h2 id="thread-picker-title" class="text-base font-semibold text-text-primary">Jump to thread</h2>
        <input
          bind:this={searchEl}
          bind:value={query}
          type="text"
          placeholder="Filter by title, project, or workspace…"
          aria-label="Filter threads"
          data-testid="thread-picker-input"
          class="mt-3 w-full text-sm rounded border border-border bg-surface-0 px-2.5 py-1.5 text-text-primary focus:outline-none focus:ring-2 focus:ring-accent/50"
        />
      </div>

      <div class="flex-1 overflow-y-auto">
        {#if visibleThreads.length === 0}
          <div class="px-5 py-4 text-xs text-text-secondary" data-testid="thread-picker-empty">
            No threads yet — create one from the sidebar.
          </div>
        {:else if hits.length === 0}
          <div class="px-5 py-4 text-xs text-text-secondary" data-testid="thread-picker-empty">
            No threads match "{query.trim()}".
          </div>
        {:else}
          <ul class="py-1" data-testid="thread-picker-results">
            {#each hits as hit, i (hit.thread.id)}
              {@const status = getThreadStatus(hit.thread.id)}
              {@const basename = projectBasename(hit.thread.projectPath)}
              <li>
                <button
                  type="button"
                  data-testid="thread-picker-hit-{hit.thread.id}"
                  onclick={() => openHit(hit.thread)}
                  aria-current={activeIndex === i}
                  class={[
                    'w-full text-left px-5 py-2 flex items-center gap-2 cursor-pointer transition-colors',
                    activeIndex === i ? 'bg-accent/15 text-text-primary' : 'hover:bg-surface-2/50 text-text-secondary',
                  ].join(' ')}
                >
                  <span class="text-[10px] font-bold px-1 py-0.5 rounded shrink-0
                      {hit.thread.provider === 'claude' ? 'bg-accent/20 text-accent' : 'bg-provider-codex/20 text-provider-codex'}" aria-hidden="true">
                    {hit.thread.provider === 'claude' ? 'C' : 'X'}
                  </span>
                  {#if status === 'idle'}
                    <span class="w-2 h-2 shrink-0" aria-hidden="true"></span>
                  {:else}
                    <span
                      class="w-2 h-2 rounded-full shrink-0 {STATUS_CLASS[status]}"
                      role="status"
                      aria-label={STATUS_LABEL[status]}
                      title={STATUS_LABEL[status]}
                      data-testid="thread-picker-status-dot"
                      data-status={status}
                    ></span>
                  {/if}
                  <span class="text-sm truncate text-text-primary flex-1 min-w-0">
                    {#each computeHighlightSegments(hit.thread.title || 'Untitled', query) as seg}
                      {#if seg.type === 'match'}
                        <mark class="bg-accent/30 text-text-primary rounded-sm px-0.5">{seg.value}</mark>
                      {:else}{seg.value}{/if}
                    {/each}
                  </span>
                  {#if hit.thread.worktreePath}
                    <span class="text-[9px] px-1 py-0.5 rounded bg-accent/15 text-accent/70 shrink-0" title="Worktree: {hit.thread.worktreePath}">worktree</span>
                  {/if}
                  {#if basename}
                    <span class="text-[10px] text-text-secondary/70 shrink-0 ml-auto truncate max-w-[12rem]">{basename}</span>
                  {/if}
                </button>
              </li>
            {/each}
          </ul>
        {/if}
      </div>

      <div class="px-5 py-2 border-t border-border text-[10px] text-text-secondary/70 flex items-center justify-between gap-3">
        <span>↑↓ to navigate · ↵ to open · Esc to close</span>
        {#if overflow > 0}
          <span data-testid="thread-picker-overflow" class="text-text-secondary/80">
            {overflow} more — refine your query.
          </span>
        {/if}
      </div>
    </div>
  </div>
{/if}
