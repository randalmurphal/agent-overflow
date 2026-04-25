<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { fly } from 'svelte/transition';
  import { ListThreadProposedPlans } from '../../stores/bindings';
  import { onItemUpsert } from '../../stores/events';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item, ProposedPlanMeta } from '../../types/models';
  import { debounce } from '../../utils/debounce';
  import { isUiRenderTraceEnabled, recordUiTrace, scheduleDomUiTrace } from '../../utils/uiRenderTrace';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  const PREVIEW_CHAR_LIMIT = 120;
  const REFRESH_DEBOUNCE_MS = 100;

  interface PlanRow {
    itemId: string;
    turnIndex: number;
    title: string;
    previewSnippet: string;
  }

  function parsePlanMeta(item: Item): ProposedPlanMeta | null {
    if (!item.payloadMeta) return null;
    try {
      return JSON.parse(item.payloadMeta) as ProposedPlanMeta;
    } catch {
      return null;
    }
  }

  function trimPreview(preview: string): string {
    const oneLine = preview.replace(/\s+/g, ' ').trim();
    if (oneLine.length <= PREVIEW_CHAR_LIMIT) return oneLine;
    return `${oneLine.slice(0, PREVIEW_CHAR_LIMIT - 1).trimEnd()}…`;
  }

  function itemsToRows(items: Item[]): PlanRow[] {
    // Backend returns newest-first ordered by (turn_index, item_index)
    // DESC, so no re-sort is necessary. A defensive filter keeps us
    // insulated if the SQL ever widens beyond proposed_plan kinds.
    return items
      .filter((item) => item.payloadKind === 'proposed_plan' && !!item.payloadId)
      .map((item) => {
        const meta = parsePlanMeta(item);
        return {
          itemId: item.id,
          turnIndex: item.turnIndex,
          title: meta?.title?.trim() || 'Proposed plan',
          previewSnippet: meta ? trimPreview(meta.preview ?? '') : '',
        };
      });
  }

  // Thread-wide plan list, fetched from the dedicated binding. This is
  // independent of the windowed `pane.items` tail — a plan emitted 200
  // turns ago must still show up in the sidebar even when it's been
  // paged out of the timeline window.
  let planRows: PlanRow[] = $state([]);
  let sidebarRoot: HTMLElement | undefined = $state(undefined);
  let visible = $derived(pane.showPlanSidebar);
  let threadId = $derived(pane.thread?.id ?? null);

  let fetchSeq = 0;
  async function refreshPlans(): Promise<void> {
    const id = threadId;
    const seq = ++fetchSeq;
    if (!id) {
      planRows = [];
      return;
    }
    try {
      const items = (await ListThreadProposedPlans(id)) as Item[] | null;
      if (seq !== fetchSeq) return;
      if (id !== threadId) return;
      planRows = itemsToRows((items ?? []).filter((item) => item.threadId === id));
    } catch (err) {
      if (seq !== fetchSeq) return;
      if (id !== threadId) return;
      console.error('PlanSidebar: ListThreadProposedPlans failed:', err);
      planRows = [];
    }
  }

  const debouncedRefresh = debounce(() => { void refreshPlans(); }, REFRESH_DEBOUNCE_MS);

  // Initial + on-thread-switch fetch.
  $effect(() => {
    // Track the thread id so a switch retriggers the effect.
    threadId;
    void refreshPlans();
  });

  $effect(() => {
    threadId;
    visible;
    planRows.length;

    if (!isUiRenderTraceEnabled()) return;
    recordUiTrace('plan-sidebar.state', {
      threadId,
      visible,
      rows: planRows.map((row) => ({
        itemId: row.itemId,
        turnIndex: row.turnIndex,
        title: row.title,
      })),
    });
    scheduleDomUiTrace('plan-sidebar', 'plan-sidebar.dom', () => ({
      threadId,
      visible,
      rows: Array.from(sidebarRoot?.querySelectorAll<HTMLElement>('[data-testid="plan-sidebar-row"]') ?? [])
        .map((el) => ({
          itemId: el.dataset.itemId ?? '',
          textPreview: (el.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 120),
        })),
    }));
  });

  let cancelItemUpsert: (() => void) | null = null;
  onMount(() => {
    cancelItemUpsert = onItemUpsert((item) => {
      if (!item.threadId) return;
      if (item.threadId !== threadId) return;
      if (item.payloadKind !== 'proposed_plan') return;
      debouncedRefresh();
    });
  });

  onDestroy(() => {
    cancelItemUpsert?.();
    debouncedRefresh.cancel();
  });

  function handleRowClick(itemId: string) {
    // Route through the pane so out-of-window plans get paged in first.
    // MessageTimeline owns the DOM scroll call and will toast if the
    // item has been deleted since the list was fetched.
    pane.requestScrollToItem(itemId);
  }

  function handleKeydown(event: KeyboardEvent, itemId: string) {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      handleRowClick(itemId);
    }
  }
</script>

{#if visible}
  <aside
    bind:this={sidebarRoot}
    transition:fly={{ x: 280, duration: 150 }}
    aria-label="Proposed Plans"
    data-testid="plan-sidebar"
    class="flex w-[280px] shrink-0 flex-col border-l border-border bg-surface-1"
  >
    <div class="flex items-center justify-between gap-2 border-b border-border px-3 py-2">
      <h3 class="text-xs font-semibold uppercase tracking-wide text-text-secondary">Proposed Plans</h3>
      <button
        type="button"
        onclick={() => pane.setShowPlanSidebar(false)}
        data-testid="plan-sidebar-close"
        aria-label="Close Plan Sidebar"
        class="rounded p-1 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        <svg class="h-3.5 w-3.5" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true">
          <path d="M2 2l8 8M10 2l-8 8" stroke-linecap="round" />
        </svg>
      </button>
    </div>

    <div class="flex-1 overflow-y-auto">
      {#if planRows.length === 0}
        <p class="px-3 py-4 text-xs text-text-secondary" data-testid="plan-sidebar-empty">
          No plans yet.
        </p>
      {:else}
        <ul class="divide-y divide-border">
          {#each planRows as row (row.itemId)}
            <li>
              <button
                type="button"
                onclick={() => handleRowClick(row.itemId)}
                onkeydown={(e) => handleKeydown(e, row.itemId)}
                data-testid="plan-sidebar-row"
                data-item-id={row.itemId}
                class="w-full px-3 py-2 text-left text-xs text-text-secondary hover:bg-surface-2/60 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
              >
                <div class="flex items-start justify-between gap-2">
                  <p class="truncate text-sm font-medium text-text-primary">{row.title}</p>
                  <span class="shrink-0 rounded bg-accent/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-accent">
                    Turn {row.turnIndex + 1}
                  </span>
                </div>
                {#if row.previewSnippet.length > 0}
                  <p class="mt-1 line-clamp-2 text-text-secondary/80">
                    {row.previewSnippet}
                  </p>
                {/if}
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  </aside>
{/if}
