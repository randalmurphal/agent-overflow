<script lang="ts">
  import { onMount, onDestroy, untrack } from 'svelte';
  import { fly } from 'svelte/transition';
  import {
    getThreadProposedPlans,
    refreshThreadProposedPlans,
    retainProposedPlanEventListener,
  } from '../../stores/proposedPlans.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item, ProposedPlanMeta } from '../../types/models';
  import {
    parseProposedPlanItemMeta,
    parseProposedPlanPayloadMeta,
  } from '../../utils/proposedPlan';
  import { isUiRenderTraceEnabled, recordUiTrace, scheduleDomUiTrace } from '../../utils/uiRenderTrace';
  import ProposedPlanCard from './ProposedPlanCard.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  interface PlanRow {
    item: Item;
    itemId: string;
    turnIndex: number;
    title: string;
    version: number;
    implemented: boolean;
    meta: ProposedPlanMeta;
  }

  function itemsToRows(items: Item[]): PlanRow[] {
    // Backend returns newest-first ordered by (turn_index, item_index)
    // DESC, so no re-sort is necessary. A defensive filter keeps us
    // insulated if the SQL ever widens beyond proposed_plan kinds.
    return items
      .filter((item) => item.payloadKind === 'proposed_plan' && !!item.payloadId)
      .map((item) => {
        const meta = parseProposedPlanPayloadMeta(item);
        const itemMeta = parseProposedPlanItemMeta(item);
        return {
          item,
          itemId: item.id,
          turnIndex: item.turnIndex,
          title: meta?.title?.trim() || 'Proposed plan',
          version: itemMeta.planVersion ?? 0,
          implemented: Boolean(itemMeta.planImplementedAt),
          meta,
        };
      });
  }

  // Thread-wide plan list, fetched from the dedicated binding. This is
  // independent of the windowed `pane.items` tail — a plan emitted 200
  // turns ago must still show up in the sidebar even when it's been
  // paged out of the timeline window.
  let planRows: PlanRow[] = $state([]);
  let selectedPlanId: string | null = $state(null);
  let sidebarRoot: HTMLElement | undefined = $state(undefined);
  let visible = $derived(pane.showPlanSidebar);
  let threadId = $derived(pane.thread?.id ?? null);
  let selectedPlan = $derived(planRows.find((row) => row.itemId === selectedPlanId) ?? planRows[0] ?? null);

  function syncPlansFromCache(): void {
    const id = threadId;
    if (!id) {
      planRows = [];
      selectedPlanId = null;
      return;
    }
    const nextRows = itemsToRows(getThreadProposedPlans(id));
    const requestedSelection = untrack(() => pane.requestedPlanSidebarItemId);
    const previousSelection = untrack(() => selectedPlanId);
    planRows = nextRows;
    if (requestedSelection && nextRows.some((row) => row.itemId === requestedSelection)) {
      selectedPlanId = requestedSelection;
      pane.clearRequestedPlanSidebarItem();
      return;
    }
    if (previousSelection && nextRows.some((row) => row.itemId === previousSelection)) {
      selectedPlanId = previousSelection;
      return;
    }
    selectedPlanId = nextRows[0]?.itemId ?? null;
  }

  // Initial + on-thread-switch fetch.
  $effect(() => {
    // Track the thread id so a switch retriggers the effect.
    const id = threadId;
    selectedPlanId = null;
    untrack(() => { void refreshThreadProposedPlans(id); });
  });

  $effect(() => {
    threadId;
    pane.requestedPlanSidebarItemId;
    getThreadProposedPlans(threadId);
    syncPlansFromCache();
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
        version: row.version,
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

  let releasePlanEvents: (() => void) | null = null;
  onMount(() => {
    releasePlanEvents = retainProposedPlanEventListener(() => threadId);
  });

  onDestroy(() => {
    releasePlanEvents?.();
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

  function handleSelect(event: Event) {
    selectedPlanId = (event.target as HTMLSelectElement).value;
    pane.clearRequestedPlanSidebarItem();
  }
</script>

{#if visible}
  <aside
    bind:this={sidebarRoot}
    transition:fly={{ x: 280, duration: 150 }}
    aria-label="Proposed Plans"
    data-testid="plan-sidebar"
    class="flex w-[440px] shrink-0 flex-col border-l border-border bg-surface-1"
  >
    <div class="flex items-center justify-between gap-2 border-b border-border px-3 py-2">
      {#if planRows.length > 1}
        <select
          aria-label="Select Plan Version"
          value={selectedPlan?.itemId ?? ''}
          onchange={handleSelect}
          class="h-7 min-w-0 rounded-md border border-border-subtle bg-surface-0 px-2 text-xs font-medium text-fg outline-none focus:border-accent focus:ring-2 focus:ring-accent/30"
          data-testid="plan-version-select"
        >
          {#each planRows as row (row.itemId)}
            <option value={row.itemId}>
              {row.version ? `Plan v${row.version}` : `Turn ${row.turnIndex + 1}`} - {row.title}
            </option>
          {/each}
        </select>
      {:else}
        <h3 class="text-xs font-semibold uppercase tracking-wide text-text-secondary">Proposed Plan</h3>
      {/if}
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
      {:else if selectedPlan}
        <div class="border-b border-border-subtle px-3 py-2">
          <button
            type="button"
            onclick={() => handleRowClick(selectedPlan.itemId)}
            onkeydown={(e) => handleKeydown(e, selectedPlan.itemId)}
            data-testid="plan-sidebar-row"
            data-item-id={selectedPlan.itemId}
            class="flex w-full items-center justify-between gap-2 rounded-md px-2 py-1.5 text-left text-xs text-text-secondary hover:bg-surface-2/60 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            <span class="truncate">
              {selectedPlan.version ? `Plan v${selectedPlan.version}` : 'Plan'} - Turn {selectedPlan.turnIndex + 1}
            </span>
            {#if selectedPlan.implemented}
              <span class="shrink-0 rounded bg-success/12 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-success">
                Implemented
              </span>
            {/if}
          </button>
        </div>
        <div class="p-3">
          {#key selectedPlan.itemId}
            <ProposedPlanCard
              {pane}
              item={selectedPlan.item}
              payloadId={selectedPlan.item.payloadId ?? ''}
              meta={selectedPlan.meta}
              fullPlan
              showReview
            />
          {/key}
        </div>
      {/if}
    </div>
  </aside>
{/if}
