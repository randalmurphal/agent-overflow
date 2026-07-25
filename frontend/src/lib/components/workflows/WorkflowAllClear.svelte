<script lang="ts">
  // The terminal sweep level (UI-SPEC §4.4): reached only by exhausting the
  // parked set. Centred ✓, "Nothing needs you", and the session's own receipt
  // summary — not an all-time total.
  //
  // R4: terse and past tense. No exclamation, no celebration copy.

  import { workflowSessionSummary, formatWorkflowCost } from '../../stores/workflowData';
  import { getWorkflowReceipts } from '../../stores/workflowRuns.svelte';
  import { closeWorkflowsOverlay, popWorkflowsOverlay } from '../../stores/workflowsOverlay.svelte';

  let summary = $derived(workflowSessionSummary(getWorkflowReceipts()));
  let cost = $derived(formatWorkflowCost(summary.costUsd));
</script>

<div class="flex min-h-[22rem] items-center justify-center px-4 py-16 text-center" data-testid="workflow-all-clear">
  <div>
    <div class="text-3xl text-success">✓</div>
    <h2 class="mt-2 text-lg font-semibold text-fg">Nothing needs you</h2>
    {#if summary.count > 0}
      <p class="mt-1 text-xs text-fg-muted" data-testid="workflow-all-clear-summary">
        {summary.count} resolved{summary.fragments ? ` — ${summary.fragments}` : ''}{cost ? ` · ${cost} reviewed` : ''}
      </p>
    {/if}
    <button
      class="mt-4 rounded-md border border-border-subtle px-3 py-1.5 text-xs text-fg-muted hover:text-fg"
      onclick={() => { if (!popWorkflowsOverlay()) closeWorkflowsOverlay(); }}
      data-testid="workflow-all-clear-back"
    >Back to workflows</button>
  </div>
</div>
