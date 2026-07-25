<script lang="ts">
  // Run-detail header block (UI-SPEC §4.1). Three rows:
  //   1. project chip · state word · sweep counter when parked
  //   2. the run title
  //   3. hint — `workflow · phase 4/5 · parked 7h · $3.10`, plus `spawned by
  //      <automation> · <trigger>` on an automation run and `→ <thread>` on a
  //      bound one.
  //
  // R1: the state word carries the only colour on this block, and only for
  // needs-human (amber) or failed (red).

  import type { WorkItem } from '../../types/workflow';
  import { formatWorkflowCost, workflowAge, workflowMetaLine } from '../../stores/workflowData';
  import { workflowRunSignal } from '../../utils/workflowRunSignal';
  import { getWorkflowAutomations } from '../../stores/workflowRuns.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import { openWorkflowThreadById } from '../../stores/workflowThreads';
  import { getThreadById } from '../../stores/threads.svelte';
  import { workflowSweepCounter } from '../../stores/workflowSweep';

  interface Props {
    item: WorkItem;
    costUsd: number;
  }
  let { item, costUsd }: Props = $props();

  let signal = $derived(workflowRunSignal(item.state, item.reason));
  let project = $derived(getProject(item.projectId)?.project);
  let counter = $derived(workflowSweepCounter(item.id));
  let boundThreadId = $derived(item.originThreadId ?? '');
  // The bound thread's title when the registry happens to hold it; workflow
  // threads are excluded from ListThreads by mode, so a bound chat thread
  // resolves and a bound workflow thread falls back to the neutral word.
  let boundThreadTitle = $derived(boundThreadId ? (getThreadById(boundThreadId)?.title || 'thread') : '');

  let automation = $derived(
    item.source === 'automation' && item.sourceRef
      ? getWorkflowAutomations(item.projectId).find((entry) => entry.id === item.sourceRef)
      : undefined,
  );

  let hint = $derived(workflowMetaLine([
    item.workflowId,
    item.phaseCount ? `phase ${item.currentPhaseOrdinal}/${item.phaseCount}` : '',
    item.state === 'running'
      ? workflowAge(item.startedAt || item.createdAt)
      : `parked ${workflowAge(item.endedAt || item.startedAt || item.createdAt)}`,
    formatWorkflowCost(costUsd),
    automation ? `spawned by ${automation.name}${automation.triggerSummary ? ` · ${automation.triggerSummary}` : ''}` : '',
  ]));
</script>

<header class="space-y-1 px-4 pb-2 pt-4" data-testid="workflow-run-header">
  <div class="flex min-w-0 items-center gap-2 text-xs">
    <span class="shrink-0 truncate text-fg-muted" data-testid="workflow-run-project">{project?.name || item.projectId}</span>
    {#if signal.label}
      <span class="shrink-0 text-fg-subtle">·</span>
      <span class={['shrink-0 font-medium', signal.tone].join(' ')} data-testid="workflow-run-state">{signal.label}</span>
    {/if}
    {#if counter}
      <span class="ml-auto shrink-0 tabular-nums text-fg-muted" data-testid="workflow-sweep-counter">
        {counter.position} of {counter.total}
      </span>
    {/if}
  </div>

  <h2 class="text-base font-semibold text-fg" data-testid="workflow-run-title">{item.goal || item.workflowId}</h2>

  <p class="text-[0.6875rem] text-fg-muted" data-testid="workflow-run-hint">
    {hint}{#if boundThreadId}<span class="px-1.5 text-fg-subtle">·</span><button
        class="text-fg-muted underline-offset-2 hover:text-fg hover:underline"
        onclick={() => { void openWorkflowThreadById(boundThreadId); }}
        data-testid="workflow-run-bound-thread"
      >→ {boundThreadTitle}</button>{/if}
  </p>
</header>
