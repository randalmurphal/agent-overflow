<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import { fly } from 'svelte/transition';
  import {
    getThreadCurrentProposedPlan,
    refreshThreadProposedPlans,
    retainProposedPlanEventListener,
  } from '../../stores/proposedPlans.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { parseProposedPlanPayloadMeta } from '../../utils/proposedPlan';
  import { isUiRenderTraceEnabled, recordUiTrace, scheduleDomUiTrace } from '../../utils/uiRenderTrace';
  import ProposedPlanCard from './ProposedPlanCard.svelte';

  interface Props {
    pane: ThreadPane;
    ownsPlanCache?: boolean;
  }

  let { pane, ownsPlanCache = true }: Props = $props();

  let sidebarRoot: HTMLElement | undefined = $state(undefined);
  let visible = $derived(pane.showPlanSidebar);
  let threadId = $derived(pane.thread?.id ?? null);
  let currentPlan = $derived(getThreadCurrentProposedPlan(threadId, pane.items));
  let currentPlanMeta = $derived(parseProposedPlanPayloadMeta(currentPlan));

  $effect(() => {
    const id = threadId;
    if (!ownsPlanCache) return;
    untrack(() => { void refreshThreadProposedPlans(id); });
  });

  $effect(() => {
    threadId;
    currentPlan?.id;
    visible;

    if (!isUiRenderTraceEnabled()) return;
    recordUiTrace('plan-sidebar.state', {
      threadId,
      visible,
      currentPlanId: currentPlan?.id ?? null,
      currentPlanTitle: currentPlanMeta.title,
    });
    scheduleDomUiTrace('plan-sidebar', 'plan-sidebar.dom', () => ({
      threadId,
      visible,
      currentPlanId: currentPlan?.id ?? null,
      textPreview: (sidebarRoot?.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 200),
    }));
  });

  let releasePlanEvents: (() => void) | null = null;
  onMount(() => {
    if (!ownsPlanCache) return;
    releasePlanEvents = retainProposedPlanEventListener(() => threadId);
  });

  onDestroy(() => {
    releasePlanEvents?.();
  });
</script>

{#if visible}
  <aside
    bind:this={sidebarRoot}
    transition:fly={{ x: 280, duration: 150 }}
    aria-label="Proposed Plan"
    data-testid="plan-sidebar"
    class="flex w-[440px] shrink-0 flex-col border-l border-border bg-surface-1"
  >
    <div class="flex items-center justify-between gap-2 border-b border-border px-3 py-2">
      <h3 class="text-xs font-semibold uppercase tracking-wide text-text-secondary">Proposed Plan</h3>
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

    <div class="flex-1 overflow-y-auto p-3">
      {#if currentPlan}
        {#key currentPlan.id}
          <ProposedPlanCard
            {pane}
            item={currentPlan}
            payloadId={currentPlan.payloadId ?? ''}
            meta={currentPlanMeta}
            fullPlan
            showReview
          />
        {/key}
      {:else}
        <p class="text-xs text-text-secondary" data-testid="plan-sidebar-empty">
          No plan yet.
        </p>
      {/if}
    </div>
  </aside>
{/if}
