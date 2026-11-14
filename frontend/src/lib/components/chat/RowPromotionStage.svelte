<!--
  Where a promoted row arrives. The pane still holds the reasoning row it
  replaces (stores/threadRowPromotions.svelte.ts), so this renders the held
  prose row below that row's activity run, opens to its height while the
  old row closes, and tells the pane when both have landed. The pane then
  installs the row, and the timeline renders it as an ordinary row in the
  box this stage opened.

  The content is laid out exactly as the installed row will be: the same
  boundary gap, divider and message, inside the same row column. The stage
  carries no `data-item-id`: the id still names the row it replaces.
-->
<script lang="ts">
  import { onMount } from 'svelte';
  import type { Item } from '../../types/models';
  import type {
    PaneSession,
    RevealRead,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import AssistantMessage from './AssistantMessage.svelte';
  import ResponseDivider from './ResponseDivider.svelte';
  import type { RowPromotionPlan } from './rowPromotionPlan';
  import { findRowPromotionElements, startRowPromotionMotion } from './rowPromotionMotion';

  let { pane, plan, row, pillLabel, pillDuration }: {
    pane: PaneSession & RevealRead & RowUiRegistry & ScrollHost;
    plan: RowPromotionPlan;
    /** The row the pane will install. */
    row: Item;
    pillLabel: string;
    pillDuration: string;
  } = $props();

  let stageEl: HTMLDivElement;
  let contentEl: HTMLDivElement;

  // A run that stays keeps its own row, so the gap above the prose is part
  // of what opens. A run that leaves hands its row to the prose, and that
  // gap is the row wrapper's margin.
  const stageGap = $derived(plan.boundary && !plan.removesRun);

  onMount(() => {
    const itemId = plan.itemId;
    const revealPane = pane;
    let completed = false;
    const complete = (): void => {
      if (completed) return;
      completed = true;
      revealPane.completeRowPromotion(itemId);
    };
    const elements = findRowPromotionElements(stageEl.closest('[data-row-index]') ?? stageEl, plan);
    if (!elements) {
      // The row left between the plan and this mount: install without motion.
      queueMicrotask(complete);
      return;
    }
    const marginFrom = Number.parseFloat(getComputedStyle(elements.row).marginTop) || 0;
    const marginTo = plan.boundary
      ? Number.parseFloat(getComputedStyle(document.documentElement).fontSize) || 16
      : 0;
    const cancel = startRowPromotionMotion({
      departing: elements.departing,
      stage: stageEl,
      content: contentEl,
      margin: plan.removesRun && marginFrom !== marginTo
        ? { element: elements.row, from: marginFrom, to: marginTo }
        : null,
      onLanded: complete,
    });
    return () => {
      cancel();
      // Unmounted before landing (the row left the mounted window): the hold
      // must not wait for the ceiling. Deferred out of the teardown flush.
      if (!completed) queueMicrotask(complete);
    };
  });
</script>

<div
  bind:this={stageEl}
  class="overflow-hidden"
  data-testid="row-promotion-stage"
  data-promotion-item-id={plan.itemId}
>
  <div bind:this={contentEl} class="flow-root">
    {#if stageGap}
      <div class="h-4" aria-hidden="true"></div>
    {/if}
    {#if plan.divider}
      <ResponseDivider final={plan.pill} label={pillLabel} duration={pillDuration} />
    {/if}
    <AssistantMessage {pane} item={row} />
  </div>
</div>
