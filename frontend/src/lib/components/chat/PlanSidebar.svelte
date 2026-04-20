<script lang="ts">
  import { fly } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item, ProposedPlanMeta } from '../../types/models';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  const PREVIEW_CHAR_LIMIT = 120;

  interface PlanRow {
    itemId: string;
    turnIndex: number;
    title: string;
    previewSnippet: string;
  }

  function parsePlanMeta(itemId: string): ProposedPlanMeta | null {
    const item = pane.items.find((entry) => entry.id === itemId);
    if (!item?.payloadMeta) return null;
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

  // Newest first. Proposed plans live on payload-bearing tool rows.
  let planRows = $derived<PlanRow[]>(
    pane.items
      .filter((item: Item) => item.payloadKind === 'proposed_plan' && !!item.payloadId)
      .slice()
      .reverse()
      .map((item) => {
        const meta = parsePlanMeta(item.id);
        return {
          itemId: item.id,
          turnIndex: item.turnIndex,
          title: meta?.title?.trim() || 'Proposed plan',
          previewSnippet: meta ? trimPreview(meta.preview ?? '') : '',
        };
      }),
  );

  let visible = $derived(pane.showPlanSidebar);

  function handleRowClick(itemId: string) {
    const target = document.querySelector(`[data-item-id="${CSS.escape(itemId)}"]`);
    if (target && typeof (target as HTMLElement).scrollIntoView === 'function') {
      (target as HTMLElement).scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
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
    transition:fly={{ x: 280, duration: 150 }}
    aria-label="Proposed plans"
    data-testid="plan-sidebar"
    class="flex w-[280px] shrink-0 flex-col border-l border-border bg-surface-1"
  >
    <div class="flex items-center justify-between gap-2 border-b border-border px-3 py-2">
      <h3 class="text-xs font-semibold uppercase tracking-wide text-text-secondary">Proposed plans</h3>
      <button
        type="button"
        onclick={() => pane.setShowPlanSidebar(false)}
        data-testid="plan-sidebar-close"
        aria-label="Close plan sidebar"
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
