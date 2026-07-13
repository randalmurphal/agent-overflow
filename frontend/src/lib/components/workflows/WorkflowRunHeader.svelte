<script lang="ts">
  import type { WorkflowItemDetail, WorkflowPaneLevel } from '../../types/workflow';
  import {
    getWorkflowDefinitions,
    getWorkflowReceipts,
    getWorkflowSweep,
    stepWorkflowSweep,
  } from '../../stores/workflowsPane.svelte';
  import { workflowAge } from '../../stores/workflowData';
  import { workflowRunSignal } from '../../utils/workflowRunSignal';

  interface Props {
    detail: WorkflowItemDetail;
    level: Extract<WorkflowPaneLevel, { kind: 'run' }>;
    projectName: string;
    projectColor?: string;
  }

  let { detail, level, projectName, projectColor = 'var(--fg-hint)' }: Props = $props();
  let item = $derived(detail.item);
  let signal = $derived(workflowRunSignal(item.state, item.reason));
  let sweep = $derived(getWorkflowSweep());
  let receipts = $derived(getWorkflowReceipts());
  let definition = $derived(getWorkflowDefinitions().find((entry) =>
    entry.projectId === item.projectId && entry.definition.id === item.workflowId,
  )?.definition);
  let latestPhase = $derived(detail.phases[detail.phases.length - 1]);
  let phaseOrdinal = $derived(definition?.phases.findIndex((phase) => phase.id === latestPhase?.phaseId) ?? -1);

  function hint(): string {
    const parts = [level.workflowLabel];
    if (phaseOrdinal >= 0 && definition) parts.push(`phase ${phaseOrdinal + 1}/${definition.phaseCount}`);
    if (item.state === 'done' && item.endedAt) parts.push(`finished ${workflowAge(item.endedAt)} ago`);
    else if ((item.state === 'needs-human' || item.state === 'failed') && (item.endedAt || item.createdAt)) {
      parts.push(`parked ${workflowAge(item.endedAt || item.createdAt)}`);
    }
    if (item.source === 'automation' && item.sourceRef) parts.push(`spawned by ${item.sourceRef}`);
    parts.push(`$${detail.usage.costUsd.toFixed(2)}`);
    return parts.join(' · ');
  }
</script>

<section class="space-y-1" data-testid="wf-run-header">
  <div class="flex min-w-0 flex-wrap items-center gap-2 text-xs">
    <span class="inline-flex min-w-0 items-center gap-1.5 rounded-full border border-border-subtle px-2 py-0.5">
      <span class="h-2 w-2 shrink-0 rounded-full" style:background-color={projectColor} aria-hidden="true"></span>
      <span class="truncate">{projectName}</span>
    </span>
    <span class={signal.signal === 'attention' ? 'text-warning' : signal.signal === 'failed' ? 'text-error' : 'text-fg-muted'} data-testid="wf-run-state">{signal.label || item.state}</span>
    {#if level.sweep && sweep.items.length > 0}
      <span class="ml-auto text-fg-muted" data-testid="wf-sweep-counter">{sweep.index + 1} of {sweep.items.length}</span>
      <span class="flex items-center gap-1" aria-label="Sweep progress" data-testid="wf-sweep-progress">
        {#each sweep.items as sweepItem}
          <span
            class={[
              'h-1.5 w-1.5 rounded-full',
              receipts.has(sweepItem.id) ? 'bg-success' : 'bg-fg-muted/30',
              sweepItem.id === item.id ? 'ring-1 ring-accent ring-offset-1 ring-offset-surface-0' : '',
            ].join(' ')}
            title={sweepItem.goal}
          ></span>
        {/each}
      </span>
      <button class="rounded border border-border-subtle px-1.5" onclick={() => stepWorkflowSweep(1)} title="Next run" data-testid="wf-sweep-next">j</button>
      <button class="rounded border border-border-subtle px-1.5" onclick={() => stepWorkflowSweep(-1)} title="Previous run" data-testid="wf-sweep-prev">k</button>
    {/if}
  </div>
  <h2 class="text-lg font-semibold text-fg">{item.goal}</h2>
  <p class="truncate text-xs text-fg-muted" title={hint()} data-testid="wf-run-hint">{hint()}</p>
</section>
