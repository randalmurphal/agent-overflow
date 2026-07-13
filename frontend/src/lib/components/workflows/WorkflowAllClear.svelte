<script lang="ts">
  import { popWorkflowTo, workflowAllClearSummary } from '../../stores/workflowsPane.svelte';
  let summary = $derived(workflowAllClearSummary());
  const labels: Record<string, string> = {
    approved: 'approved',
    answered: 'answered',
    'handed-off': 'handed off',
    merged: 'merged',
    're-enqueued': 're-enqueued',
    discarded: 'discarded',
    pr: 'PR created',
    removed: 'removed',
  };
  const order = Object.keys(labels);
  let fragments = $derived(order
    .filter((kind) => (summary.byKind[kind] ?? 0) > 0)
    .map((kind) => `${summary.byKind[kind]} ${labels[kind]}`)
    .join(' · '));
</script>

<div class="flex min-h-[360px] items-center justify-center p-6 text-center" data-testid="wf-all-clear">
  <div>
    <div class="text-3xl text-success">✓</div>
    <h2 class="mt-2 text-lg font-semibold">Nothing needs you</h2>
    <p class="mt-1 text-xs text-fg-muted">{summary.count} resolved{fragments ? ` — ${fragments}` : ''} · ${summary.costUsd.toFixed(2)} reviewed</p>
    <button class="mt-4 rounded-md border border-border-subtle px-3 py-1.5 text-xs hover:bg-surface-2" onclick={() => popWorkflowTo(0)} data-testid="wf-all-clear-back">Back to workflows</button>
  </div>
</div>
